package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/domain/finding"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/plan"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/migrations"
)

// M6.2-B2 发现 / 复测 / 报告接口集成测试。
//
// 路由由 handler.go 统一注册，因此这里直接调用 Options 的处理器方法，
// 用 SetPathValue 注入路径参数，不依赖 mux 与路由是否已经注册。

var (
	findingsHTTPOnce sync.Once
	findingsHTTPDB   *pg.DB
	findingsHTTPErr  error
)

// findingsHTTPEnv 准备真实 PostgreSQL 环境，并为每个测试创建独立租户。
func findingsHTTPEnv(t *testing.T) (*pg.Store, int64) {
	t.Helper()
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过发现/复测/报告集成测试")
	}
	findingsHTTPOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		findingsHTTPDB, findingsHTTPErr = pg.Open(ctx, pg.Options{
			DSN: dsn, MaxConns: 6, MinConns: 1, ConnectTimeout: 10 * time.Second,
		})
		if findingsHTTPErr != nil {
			return
		}
		_, findingsHTTPErr = migrate.Apply(ctx, findingsHTTPDB.Pool(), migrations.FS)
	})
	if findingsHTTPErr != nil {
		t.Fatalf("准备测试数据库失败: %v", findingsHTTPErr)
	}

	tenantID := time.Now().UnixNano()
	ctx := context.Background()
	if _, err := findingsHTTPDB.Pool().Exec(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
		tenantID, fmt.Sprintf("findings-test-%d", tenantID), "发现接口测试租户"); err != nil {
		t.Fatalf("创建测试租户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := findingsHTTPDB.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = $1`, tenantID); err != nil {
			t.Logf("清理测试租户失败（可忽略）: %v", err)
		}
	})
	return pg.NewStore(findingsHTTPDB), tenantID
}

// findingsOptions 构造只依赖 Store 的 Options，足够调用发现/复测/报告处理器。
func findingsOptions(store *pg.Store, tenantID int64) Options {
	return Options{Store: store, TenantID: tenantID}
}

// mustExtraTenant 创建一个额外租户，用于跨租户隔离断言。
func mustExtraTenant(t *testing.T) int64 {
	t.Helper()
	tenantID := time.Now().UnixNano() + 1
	if _, err := findingsHTTPDB.Pool().Exec(context.Background(),
		`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
		tenantID, fmt.Sprintf("findings-extra-%d", tenantID), "发现接口隔离租户"); err != nil {
		t.Fatalf("创建隔离租户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = findingsHTTPDB.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	return tenantID
}

func mustMission(t *testing.T, store *pg.Store, tenantID int64, name string) pg.Mission {
	t.Helper()
	mission, err := store.CreateMission(context.Background(), tenantID, pg.MissionInput{Name: name, Goal: "验证接口"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	return mission
}

// invokeHandler 直接调用处理器方法，并通过 SetPathValue 注入路径参数。
func invokeHandler(t *testing.T, h http.HandlerFunc, method, target, body string, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range pathValues {
		req.SetPathValue(key, value)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// decodeResponse 把 JSON 响应解码为 T；失败时带上响应体便于排障。
func decodeResponse[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v，body=%s", err, rec.Body.String())
	}
	return out
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("状态码 = %d，期望 %d，body=%s", rec.Code, want, rec.Body.String())
	}
}

func requireErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析错误响应失败: %v，body=%s", err, rec.Body.String())
	}
	if env.Error.Code != want {
		t.Fatalf("错误码 = %q，期望 %q，body=%s", env.Error.Code, want, rec.Body.String())
	}
}

// mustSnapshot 在任务下创建一条证据快照（正文哈希不需要真实文件，接口层只引用）。
func mustSnapshot(t *testing.T, store *pg.Store, tenantID, missionID int64) pg.EvidenceSnapshot {
	t.Helper()
	ref := evidence.BlobRef{Hash: strings.Repeat("a", 64), Size: 16}
	snap, err := store.CreateEvidenceSnapshot(context.Background(), tenantID, missionID,
		"http_probe", ref, ref, strings.Repeat("b", 64), "json-keys", time.Now().UTC())
	if err != nil {
		t.Fatalf("创建证据快照失败: %v", err)
	}
	return snap
}

// mustCreateFindingWith 直接通过 Store 建发现，供不需要走 HTTP 的用例使用。
func mustCreateFindingWith(t *testing.T, store *pg.Store, tenantID, missionID int64, title string) pg.Finding {
	t.Helper()
	created, err := store.CreateFinding(context.Background(), tenantID, missionID, pg.FindingInput{
		Title: title, Severity: finding.SeverityHigh, Detail: "详情",
	})
	if err != nil {
		t.Fatalf("创建发现失败: %v", err)
	}
	return created
}

func TestFindingsCRUDAndFilters(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := findingsOptions(store, tenantID)
	mission := mustMission(t, store, tenantID, "发现 CRUD 任务")
	ctx := context.Background()

	identity, err := asset.Normalize(asset.KindRootDomain, "crud.example.com")
	if err != nil {
		t.Fatalf("归一化资产身份失败: %v", err)
	}
	assetRow, err := store.UpsertAsset(ctx, tenantID, pg.AssetInput{
		Type: asset.KindRootDomain, Label: "crud.example.com", Identities: []asset.Identity{identity},
	})
	if err != nil {
		t.Fatalf("创建资产失败: %v", err)
	}
	node, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateOpen, Title: "SQL 注入验证", Body: "响应报错泄漏 SQL",
	})
	if err != nil {
		t.Fatalf("创建作战图节点失败: %v", err)
	}

	missionPath := map[string]string{"id": strconv.FormatInt(mission.ID, 10)}

	// 创建：201，初始状态 open，资产/节点关联生效。
	createBody := fmt.Sprintf(`{"title":"SQL 注入","severity":"high","detail":"布尔盲注","asset_id":%d,"node_id":%d}`,
		assetRow.ID, node.ID)
	rec := invokeHandler(t, opts.handleCreateFinding, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings", createBody, missionPath)
	requireStatus(t, rec, http.StatusCreated)
	created := decodeResponse[pg.Finding](t, rec)
	if created.ID == 0 || created.Status != "open" || created.Severity != "high" {
		t.Fatalf("创建结果不符合预期: %+v", created)
	}
	if created.AssetID == nil || *created.AssetID != assetRow.ID {
		t.Errorf("asset_id = %v，期望 %d", created.AssetID, assetRow.ID)
	}
	if created.NodeID == nil || *created.NodeID != node.ID {
		t.Errorf("node_id = %v，期望 %d", created.NodeID, node.ID)
	}
	if created.EvidenceCount != 0 {
		t.Errorf("初始 evidence_count = %d，期望 0", created.EvidenceCount)
	}
	findingPath := map[string]string{"id": strconv.FormatInt(created.ID, 10)}

	// 详情：字段平铺 + 空证据列表（不是 null）。
	rec = invokeHandler(t, opts.handleGetFinding, http.MethodGet,
		"/api/v1/findings/"+findingPath["id"], "", findingPath)
	requireStatus(t, rec, http.StatusOK)
	detail := decodeResponse[findingDetailResponse](t, rec)
	if detail.ID != created.ID || detail.Title != "SQL 注入" {
		t.Fatalf("详情不符合预期: %+v", detail)
	}
	if detail.Evidence == nil || len(detail.Evidence) != 0 {
		t.Errorf("证据列表应为非 nil 空切片，实际: %+v", detail.Evidence)
	}

	// 列表与过滤。
	listPath := "/api/v1/missions/" + missionPath["id"] + "/findings"
	rec = invokeHandler(t, opts.handleListFindings, http.MethodGet, listPath, "", missionPath)
	requireStatus(t, rec, http.StatusOK)
	list := decodeResponse[struct {
		Findings []pg.Finding `json:"findings"`
	}](t, rec)
	if len(list.Findings) != 1 || list.Findings[0].ID != created.ID {
		t.Fatalf("列表不符合预期: %+v", list.Findings)
	}
	for _, tc := range []struct {
		query    string
		wantRows int
	}{
		{"?severity=high", 1},
		{"?severity=low", 0},
		{"?status=open", 1},
		{"?status=closed", 0},
		{"?severity=high&status=open", 1},
	} {
		rec = invokeHandler(t, opts.handleListFindings, http.MethodGet, listPath+tc.query, "", missionPath)
		requireStatus(t, rec, http.StatusOK)
		got := decodeResponse[struct {
			Findings []pg.Finding `json:"findings"`
		}](t, rec)
		if len(got.Findings) != tc.wantRows {
			t.Errorf("%s 返回 %d 行，期望 %d", tc.query, len(got.Findings), tc.wantRows)
		}
		if tc.wantRows == 0 && !strings.Contains(rec.Body.String(), `"findings":[]`) {
			t.Errorf("%s 空结果应输出 []，实际: %s", tc.query, rec.Body.String())
		}
	}
	rec = invokeHandler(t, opts.handleListFindings, http.MethodGet, listPath+"?severity=urgent", "", missionPath)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleListFindings, http.MethodGet, listPath+"?status=bogus", "", missionPath)
	requireStatus(t, rec, http.StatusBadRequest)

	// 局部更新：改标题/严重级别/详情，并显式清空资产。
	rec = invokeHandler(t, opts.handleUpdateFinding, http.MethodPatch,
		"/api/v1/findings/"+findingPath["id"],
		`{"title":"SQL 注入（已确认）","severity":"critical","detail":"已复现","asset_id":null}`, findingPath)
	requireStatus(t, rec, http.StatusOK)
	updated := decodeResponse[pg.Finding](t, rec)
	if updated.Title != "SQL 注入（已确认）" || updated.Severity != "critical" || updated.Detail != "已复现" {
		t.Fatalf("更新结果不符合预期: %+v", updated)
	}
	if updated.AssetID != nil {
		t.Errorf("asset_id = %v，期望被清空", updated.AssetID)
	}
	if updated.Status != "open" {
		t.Errorf("状态 = %s，期望保持 open", updated.Status)
	}

	// 非法更新：未知严重级别 / 空标题 / 非法 asset_id。
	rec = invokeHandler(t, opts.handleUpdateFinding, http.MethodPatch,
		"/api/v1/findings/"+findingPath["id"], `{"severity":"urgent"}`, findingPath)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleUpdateFinding, http.MethodPatch,
		"/api/v1/findings/"+findingPath["id"], `{"title":"   "}`, findingPath)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleUpdateFinding, http.MethodPatch,
		"/api/v1/findings/"+findingPath["id"], `{"asset_id":0}`, findingPath)
	requireStatus(t, rec, http.StatusBadRequest)

	// 引用不存在的资产：400（请求体引用错误，不是任务不存在）。
	rec = invokeHandler(t, opts.handleCreateFinding, http.MethodPost, listPath,
		`{"title":"坏资产","asset_id":999999999}`, missionPath)
	requireStatus(t, rec, http.StatusBadRequest)

	// 跨租户：同一进程换租户上下文后，读写都应 404。
	otherTenant := mustExtraTenant(t)
	otherOpts := findingsOptions(store, otherTenant)
	rec = invokeHandler(t, otherOpts.handleGetFinding, http.MethodGet,
		"/api/v1/findings/"+findingPath["id"], "", findingPath)
	requireStatus(t, rec, http.StatusNotFound)
	rec = invokeHandler(t, otherOpts.handleListFindings, http.MethodGet, listPath, "", missionPath)
	requireStatus(t, rec, http.StatusNotFound)
}

func TestFindingsTransitions(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := findingsOptions(store, tenantID)
	mission := mustMission(t, store, tenantID, "发现状态机任务")

	create := func(title string) pg.Finding {
		t.Helper()
		rec := invokeHandler(t, opts.handleCreateFinding, http.MethodPost,
			"/api/v1/missions/"+strconv.FormatInt(mission.ID, 10)+"/findings",
			`{"title":"`+title+`"}`, map[string]string{"id": strconv.FormatInt(mission.ID, 10)})
		requireStatus(t, rec, http.StatusCreated)
		return decodeResponse[pg.Finding](t, rec)
	}
	transition := func(id int64, body string) *httptest.ResponseRecorder {
		t.Helper()
		return invokeHandler(t, opts.handleTransitionFinding, http.MethodPost,
			"/api/v1/findings/"+strconv.FormatInt(id, 10)+"/status", body,
			map[string]string{"id": strconv.FormatInt(id, 10)})
	}

	// 合法链路：open → confirmed → retest → fixed → retest → closed。
	f := create("合法链路")
	for _, want := range []string{"confirmed", "retest", "fixed", "retest", "closed"} {
		rec := transition(f.ID, `{"status":"`+want+`"}`)
		requireStatus(t, rec, http.StatusOK)
		got := decodeResponse[pg.Finding](t, rec)
		if string(got.Status) != want {
			t.Fatalf("状态 = %s，期望 %s", got.Status, want)
		}
	}
	// 终态不能再迁移：409。
	rec := transition(f.ID, `{"status":"fixed"}`)
	requireStatus(t, rec, http.StatusConflict)
	requireErrorCode(t, rec, "conflict")

	// 非法迁移 open → fixed：409。
	openToFixed := create("非法迁移")
	rec = transition(openToFixed.ID, `{"status":"fixed"}`)
	requireStatus(t, rec, http.StatusConflict)
	requireErrorCode(t, rec, "conflict")

	// 同状态不算迁移：409。
	same := create("同状态")
	rec = transition(same.ID, `{"status":"open"}`)
	requireStatus(t, rec, http.StatusConflict)

	// 未知目标状态：400。
	rec = transition(same.ID, `{"status":"bogus"}`)
	requireStatus(t, rec, http.StatusBadRequest)

	// 不存在的发现：404。
	rec = transition(999999999, `{"status":"confirmed"}`)
	requireStatus(t, rec, http.StatusNotFound)
}

func TestFindingEvidence(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := findingsOptions(store, tenantID)
	mission := mustMission(t, store, tenantID, "证据关联任务")
	otherMission := mustMission(t, store, tenantID, "另一个任务")
	finding := mustCreateFindingWith(t, store, tenantID, mission.ID, "证据关联发现")

	snapshot := mustSnapshot(t, store, tenantID, mission.ID)
	otherSnapshot := mustSnapshot(t, store, tenantID, otherMission.ID)
	findingPath := map[string]string{"id": strconv.FormatInt(finding.ID, 10)}

	// 绑定：201；重复绑定 409。
	rec := invokeHandler(t, opts.handleAddFindingEvidence, http.MethodPost,
		"/api/v1/findings/"+findingPath["id"]+"/evidence",
		fmt.Sprintf(`{"snapshot_id":%d}`, snapshot.ID), findingPath)
	requireStatus(t, rec, http.StatusCreated)
	rec = invokeHandler(t, opts.handleAddFindingEvidence, http.MethodPost,
		"/api/v1/findings/"+findingPath["id"]+"/evidence",
		fmt.Sprintf(`{"snapshot_id":%d}`, snapshot.ID), findingPath)
	requireStatus(t, rec, http.StatusConflict)

	// 列表：1 条；详情 evidence_count 同步为 1。
	rec = invokeHandler(t, opts.handleListFindingEvidence, http.MethodGet,
		"/api/v1/findings/"+findingPath["id"]+"/evidence", "", findingPath)
	requireStatus(t, rec, http.StatusOK)
	evidenceList := decodeResponse[struct {
		Evidence []pg.FindingEvidence `json:"evidence"`
	}](t, rec)
	if len(evidenceList.Evidence) != 1 || evidenceList.Evidence[0].SnapshotID != snapshot.ID {
		t.Fatalf("证据列表不符合预期: %+v", evidenceList.Evidence)
	}
	detail := func() findingDetailResponse {
		t.Helper()
		rec := invokeHandler(t, opts.handleGetFinding, http.MethodGet,
			"/api/v1/findings/"+findingPath["id"], "", findingPath)
		requireStatus(t, rec, http.StatusOK)
		return decodeResponse[findingDetailResponse](t, rec)
	}
	if got := detail(); got.EvidenceCount != 1 || len(got.Evidence) != 1 {
		t.Fatalf("详情 evidence_count = %d、evidence = %d，期望 1/1", got.EvidenceCount, len(got.Evidence))
	}

	// 引用其它任务的快照、或不存在快照：400。
	rec = invokeHandler(t, opts.handleAddFindingEvidence, http.MethodPost,
		"/api/v1/findings/"+findingPath["id"]+"/evidence",
		fmt.Sprintf(`{"snapshot_id":%d}`, otherSnapshot.ID), findingPath)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleAddFindingEvidence, http.MethodPost,
		"/api/v1/findings/"+findingPath["id"]+"/evidence",
		`{"snapshot_id":999999999}`, findingPath)
	requireStatus(t, rec, http.StatusBadRequest)

	// 给不存在的发现绑定证据：404。
	rec = invokeHandler(t, opts.handleAddFindingEvidence, http.MethodPost,
		"/api/v1/findings/999999999/evidence",
		fmt.Sprintf(`{"snapshot_id":%d}`, snapshot.ID), map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)

	// 解绑：204；再解绑 404；列表回到空。
	deletePath := map[string]string{
		"id":          findingPath["id"],
		"snapshot_id": strconv.FormatInt(snapshot.ID, 10),
	}
	rec = invokeHandler(t, opts.handleRemoveFindingEvidence, http.MethodDelete,
		"/api/v1/findings/"+findingPath["id"]+"/evidence/"+deletePath["snapshot_id"], "", deletePath)
	requireStatus(t, rec, http.StatusNoContent)
	rec = invokeHandler(t, opts.handleRemoveFindingEvidence, http.MethodDelete,
		"/api/v1/findings/"+findingPath["id"]+"/evidence/"+deletePath["snapshot_id"], "", deletePath)
	requireStatus(t, rec, http.StatusNotFound)
	if got := detail(); got.EvidenceCount != 0 {
		t.Fatalf("解绑后 evidence_count = %d，期望 0", got.EvidenceCount)
	}
}

func TestRetests(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := findingsOptions(store, tenantID)
	mission := mustMission(t, store, tenantID, "复测任务")
	otherMission := mustMission(t, store, tenantID, "其它任务")
	finding := mustCreateFindingWith(t, store, tenantID, mission.ID, "待复测发现")
	otherFinding := mustCreateFindingWith(t, store, tenantID, otherMission.ID, "跨任务发现")
	missionPath := map[string]string{"id": strconv.FormatInt(mission.ID, 10)}

	// 发起复测：201，初始 pending。
	rec := invokeHandler(t, opts.handleCreateRetest, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/retests",
		fmt.Sprintf(`{"finding_id":%d,"note":"验证修复"}`, finding.ID), missionPath)
	requireStatus(t, rec, http.StatusCreated)
	retest := decodeResponse[pg.Retest](t, rec)
	if retest.Status != "pending" || retest.FindingID != finding.ID {
		t.Fatalf("复测创建结果不符合预期: %+v", retest)
	}
	if retest.RequestedBy != "user" {
		t.Errorf("requested_by = %q，期望默认 user", retest.RequestedBy)
	}
	retestPath := map[string]string{"id": strconv.FormatInt(retest.ID, 10)}

	// 状态流转：pending → running → passed。
	rec = invokeHandler(t, opts.handlePatchRetest, http.MethodPatch,
		"/api/v1/retests/"+retestPath["id"], `{"status":"running"}`, retestPath)
	requireStatus(t, rec, http.StatusOK)
	running := decodeResponse[pg.Retest](t, rec)
	if running.Status != "running" || running.StartedAt == nil {
		t.Fatalf("running 状态不符合预期: %+v", running)
	}
	rec = invokeHandler(t, opts.handlePatchRetest, http.MethodPatch,
		"/api/v1/retests/"+retestPath["id"], `{"status":"passed","note":"已确认修复"}`, retestPath)
	requireStatus(t, rec, http.StatusOK)
	passed := decodeResponse[pg.Retest](t, rec)
	if passed.Status != "passed" || passed.FinishedAt == nil || passed.Note != "已确认修复" {
		t.Fatalf("passed 状态不符合预期: %+v", passed)
	}

	// 终态再迁移、以及非法目标状态。
	rec = invokeHandler(t, opts.handlePatchRetest, http.MethodPatch,
		"/api/v1/retests/"+retestPath["id"], `{"status":"running"}`, retestPath)
	requireStatus(t, rec, http.StatusConflict)
	requireErrorCode(t, rec, "conflict")
	rec = invokeHandler(t, opts.handlePatchRetest, http.MethodPatch,
		"/api/v1/retests/"+retestPath["id"], `{"status":"bogus"}`, retestPath)
	requireStatus(t, rec, http.StatusBadRequest)

	// 读取与列表。
	rec = invokeHandler(t, opts.handleGetRetest, http.MethodGet,
		"/api/v1/retests/"+retestPath["id"], "", retestPath)
	requireStatus(t, rec, http.StatusOK)
	rec = invokeHandler(t, opts.handleListRetests, http.MethodGet,
		"/api/v1/missions/"+missionPath["id"]+"/retests", "", missionPath)
	requireStatus(t, rec, http.StatusOK)
	list := decodeResponse[struct {
		Retests []pg.Retest `json:"retests"`
	}](t, rec)
	if len(list.Retests) != 1 || list.Retests[0].ID != retest.ID {
		t.Fatalf("复测列表不符合预期: %+v", list.Retests)
	}

	// 跨任务发现：400；不存在发现：404；缺失 finding_id：400。
	rec = invokeHandler(t, opts.handleCreateRetest, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/retests",
		fmt.Sprintf(`{"finding_id":%d}`, otherFinding.ID), missionPath)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleCreateRetest, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/retests", `{"finding_id":999999999}`, missionPath)
	requireStatus(t, rec, http.StatusNotFound)
	rec = invokeHandler(t, opts.handleCreateRetest, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/retests", `{"finding_id":0}`, missionPath)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleGetRetest, http.MethodGet,
		"/api/v1/retests/999999999", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
}

func TestMissionReport(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := findingsOptions(store, tenantID)
	ctx := context.Background()
	mission := mustMission(t, store, tenantID, "季度报告任务")
	if err := store.SetMissionBudget(ctx, tenantID, mission.ID, 100000, 20); err != nil {
		t.Fatalf("设置预算失败: %v", err)
	}

	// 发现：两条不同严重级别。
	if _, err := store.CreateFinding(ctx, tenantID, mission.ID, pg.FindingInput{
		Title: "SQL 注入", Severity: "critical", Detail: "UNION 注入可读全表",
	}); err != nil {
		t.Fatalf("创建发现失败: %v", err)
	}
	if _, err := store.CreateFinding(ctx, tenantID, mission.ID, pg.FindingInput{
		Title: "反射型 XSS", Severity: "high",
	}); err != nil {
		t.Fatalf("创建发现失败: %v", err)
	}

	// 计划步骤。
	if _, err := store.UpsertPlanSteps(ctx, tenantID, mission.ID, []plan.Step{
		{Key: "recon", Title: "信息收集"},
		{Key: "exploit", Title: "漏洞利用", DependsOn: []string{"recon"}},
	}); err != nil {
		t.Fatalf("写入计划步骤失败: %v", err)
	}

	// 事实节点（应进入报告）与目标节点（不应进入报告）。
	if _, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateOpen,
		Title: "目标使用 nginx", Body: "响应头 Server: nginx/1.25",
	}); err != nil {
		t.Fatalf("创建事实节点失败: %v", err)
	}
	if _, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeGoal, State: graph.StateOpen,
		Title: "目标节点不应出现在事实列表", Body: "只有 fact 节点进入报告",
	}); err != nil {
		t.Fatalf("创建目标节点失败: %v", err)
	}

	// 证据统计：一条流量 + 一条快照。
	if _, err := store.BeginTraffic(ctx, tenantID, mission.ID, nil, "http_probe",
		evidence.BlobRef{Hash: strings.Repeat("c", 64), Size: 8}, "json-keys"); err != nil {
		t.Fatalf("创建流量记录失败: %v", err)
	}
	mustSnapshot(t, store, tenantID, mission.ID)

	missionPath := map[string]string{"id": strconv.FormatInt(mission.ID, 10)}
	rec := invokeHandler(t, opts.handleMissionReport, http.MethodGet,
		"/api/v1/missions/"+missionPath["id"]+"/report", "", missionPath)
	requireStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Fatalf("Content-Type = %q，期望 text/markdown; charset=utf-8", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# 任务报告：季度报告任务",
		"## 任务信息",
		"任务目标：",
		"Token 预算：100000",
		"步骤预算：20",
		"## 发现（2）",
		"SQL 注入",
		"反射型 XSS",
		"CRITICAL",
		"HIGH",
		"## 计划步骤（2）",
		"recon",
		"exploit",
		"## 事实节点（1）",
		"目标使用 nginx",
		"nginx/1.25",
		"## 证据统计",
		"流量采集记录：1 条",
		"证据快照：1 条",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("报告缺少片段 %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "目标节点不应出现在事实列表") {
		t.Errorf("报告只应包含 fact 节点，不应包含 goal 节点\n%s", body)
	}

	// 空任务：每个空段落都必须显式输出「无」。
	emptyMission := mustMission(t, store, tenantID, "空报告任务")
	emptyPath := map[string]string{"id": strconv.FormatInt(emptyMission.ID, 10)}
	rec = invokeHandler(t, opts.handleMissionReport, http.MethodGet,
		"/api/v1/missions/"+emptyPath["id"]+"/report", "", emptyPath)
	requireStatus(t, rec, http.StatusOK)
	emptyBody := rec.Body.String()
	for _, want := range []string{
		"## 发现（0）",
		"无：本次任务尚未登记发现",
		"## 计划步骤（0）",
		"无：任务尚未生成计划步骤",
		"## 事实节点（0）",
		"无：任务尚未记录事实节点",
	} {
		if !strings.Contains(emptyBody, want) {
			t.Errorf("空任务报告缺少片段 %q\n%s", want, emptyBody)
		}
	}

	// 任务不存在：404。
	rec = invokeHandler(t, opts.handleMissionReport, http.MethodGet,
		"/api/v1/missions/999999999/report", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
}

func TestFindingsEmptyListsAndErrors(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := findingsOptions(store, tenantID)
	mission := mustMission(t, store, tenantID, "空列表任务")
	missionPath := map[string]string{"id": strconv.FormatInt(mission.ID, 10)}

	// 空发现 / 空复测都必须是 [] 而不是 null。
	rec := invokeHandler(t, opts.handleListFindings, http.MethodGet,
		"/api/v1/missions/"+missionPath["id"]+"/findings", "", missionPath)
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"findings":[]`) {
		t.Errorf("空发现列表应输出 []，实际: %s", rec.Body.String())
	}
	var findingsList struct {
		Findings []pg.Finding `json:"findings"`
	}
	if raw := rec.Body.Bytes(); json.Unmarshal(raw, &findingsList) != nil || findingsList.Findings == nil {
		t.Errorf("空发现列表不应解码为 nil: %s", raw)
	}
	rec = invokeHandler(t, opts.handleListRetests, http.MethodGet,
		"/api/v1/missions/"+missionPath["id"]+"/retests", "", missionPath)
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"retests":[]`) {
		t.Errorf("空复测列表应输出 []，实际: %s", rec.Body.String())
	}

	// Store 未装配：503。
	unready := Options{}
	rec = invokeHandler(t, unready.handleListFindings, http.MethodGet,
		"/api/v1/missions/1/findings", "", map[string]string{"id": "1"})
	requireStatus(t, rec, http.StatusServiceUnavailable)
	rec = invokeHandler(t, unready.handleMissionReport, http.MethodGet,
		"/api/v1/missions/1/report", "", map[string]string{"id": "1"})
	requireStatus(t, rec, http.StatusServiceUnavailable)

	// 路径参数非法：400；任务不存在：404；发现不存在：404。
	rec = invokeHandler(t, opts.handleListFindings, http.MethodGet,
		"/api/v1/missions/not-a-number/findings", "", map[string]string{"id": "not-a-number"})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleListFindings, http.MethodGet,
		"/api/v1/missions/999999999/findings", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
	rec = invokeHandler(t, opts.handleGetFinding, http.MethodGet,
		"/api/v1/findings/999999999", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)

	// 请求体非法：创建发现。
	badBodies := []string{
		`{`,
		`{}`,
		`{"title":"   "}`,
		`{"title":"合法标题","severity":"urgent"}`,
	}
	for _, body := range badBodies {
		rec = invokeHandler(t, opts.handleCreateFinding, http.MethodPost,
			"/api/v1/missions/"+missionPath["id"]+"/findings", body, missionPath)
		requireStatus(t, rec, http.StatusBadRequest)
	}
}
