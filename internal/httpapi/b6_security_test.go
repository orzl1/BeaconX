package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/app/executor"
	"github.com/orzl1/BeaconX/internal/app/tools"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B6 安全收口测试：重放走策略门、证据密钥轮换、审计覆盖、节点唯一性。
//
// 依赖真实 PostgreSQL + 临时证据目录；未设置 BEACONX_TEST_PG_DSN 时跳过。

// b6Key 生成确定性 32 字节测试密钥。
func b6Key(seed byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

// b6Setup 准备 Store + Evidence + 独立租户 + 证据目录（便于用新旧密钥重新打开）。
func b6Setup(t *testing.T) (*pg.Store, *evidence.Store, int64, string) {
	t.Helper()
	store, tenantID := findingsHTTPEnv(t)
	dir := t.TempDir()
	ev, err := evidence.New(evidence.Config{Dir: dir, Key: b6Key(1)})
	if err != nil {
		t.Fatalf("创建证据存储失败: %v", err)
	}
	return store, ev, tenantID, dir
}

// b6Executor 构造接线的执行器（Allow 默认动作、短审批超时、带证据采集）。
func b6Executor(t *testing.T, store *pg.Store, ev *evidence.Store, tenantID int64) *executor.Executor {
	t.Helper()
	registry, err := tools.NewRegistry(tools.NewHTTPProbe())
	if err != nil {
		t.Fatalf("构造工具注册表失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return executor.New(store, registry, executor.Config{
		TenantID:      tenantID,
		DefaultAction: policy.Allow,
		AskTimeout:    100 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
		Evidence:      ev,
	}, logger)
}

// TestB6ReplayThroughExecutor 验证重放必须经过策略门：允许→200、deny→403、未接线→503。
func TestB6ReplayThroughExecutor(t *testing.T) {
	store, ev, tenantID, _ := b6Setup(t)
	mission := mustMission(t, store, tenantID, "重放策略门任务")

	hit := make(chan struct{}, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()

	args := `{"url":"` + target.URL + `","method":"GET"}`
	record := b5trafficCreateRecord(t, store, ev, tenantID, mission.ID, "http_probe", args, "HTTP 200\n\nok", 200)
	recordPath := map[string]string{"id": jsonNumber(record.ID)}

	execInstance := b6Executor(t, store, ev, tenantID)
	opts := Options{Store: store, Evidence: ev, TenantID: tenantID, Executor: execInstance}

	rec := invokeHandler(t, opts.handleTrafficReplay, http.MethodPost,
		"/api/v1/traffic/"+recordPath["id"]+"/replay", "", recordPath)
	requireStatus(t, rec, http.StatusOK)
	var replay struct {
		PolicyGated bool   `json:"policy_gated"`
		TrafficID   int64  `json:"traffic_id"`
		Output      string `json:"output"`
		Status      int    `json:"status"`
	}
	b5trafficDecodeInto(t, rec, &replay)
	if !replay.PolicyGated || !strings.Contains(replay.Output, "HTTP 200") || replay.Status != 200 {
		t.Fatalf("重放结果不符合预期: %+v", replay)
	}
	if replay.TrafficID == 0 {
		t.Error("executor 采集应产生新的 traffic 行")
	}
	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("重放请求未到达目标")
	}

	// 未接线 Executor：503（不允许旁路）。
	noExec := Options{Store: store, Evidence: ev, TenantID: tenantID}
	rec = invokeHandler(t, noExec.handleTrafficReplay, http.MethodPost,
		"/api/v1/traffic/"+recordPath["id"]+"/replay", "", recordPath)
	requireStatus(t, rec, http.StatusServiceUnavailable)
	requireErrorCode(t, rec, "executor_unavailable")

	// 构造 deny 规则：重放必须被策略门拒绝并写审计。
	if _, err := store.CreatePolicyRule(context.Background(), tenantID, pg.PolicyRuleInput{
		Name: "b6 禁止重放", Tool: "http_probe", ArgPattern: "", Action: policy.Deny,
		Priority: 1000, Enabled: true,
	}); err != nil {
		t.Fatalf("创建 deny 规则失败: %v", err)
	}
	rec = invokeHandler(t, opts.handleTrafficReplay, http.MethodPost,
		"/api/v1/traffic/"+recordPath["id"]+"/replay", "", recordPath)
	requireStatus(t, rec, http.StatusForbidden)
	requireErrorCode(t, rec, "policy_denied")

	audits, err := store.ListAudit(context.Background(), tenantID, mission.ID, 50)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	denied := false
	for _, entry := range audits {
		if entry.Tool == "http_probe" && entry.Outcome == "denied_by_rule" {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("策略拒绝应写入审计，实际: %+v", audits)
	}
}

// TestB6EvidenceRotate 验证密钥轮换：同一 Store 可读、新 key 可读、旧 key 失败、参数校验。
func TestB6EvidenceRotate(t *testing.T) {
	store, ev, tenantID, dir := b6Setup(t)

	refs := []evidence.BlobRef{
		b5trafficRef(t, ev, "第一段"),
		b5trafficRef(t, ev, "第二段"),
		b5trafficRef(t, ev, "third"),
	}
	opts := Options{Store: store, Evidence: ev, TenantID: tenantID}
	newKey := b6Key(9)
	rotateBody := `{"new_key_hex":"` + hex.EncodeToString(newKey) + `"}`

	// 参数校验：缺失/非法 hex → 400。
	rec := invokeHandler(t, opts.handleEvidenceRotate, http.MethodPost, "/api/v1/evidence/rotate", `{}`, nil)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleEvidenceRotate, http.MethodPost, "/api/v1/evidence/rotate",
		`{"new_key_hex":"zzzz"}`, nil)
	requireStatus(t, rec, http.StatusBadRequest)

	// 成功轮换：rotated >= blob 数。
	rec = invokeHandler(t, opts.handleEvidenceRotate, http.MethodPost, "/api/v1/evidence/rotate", rotateBody, nil)
	requireStatus(t, rec, http.StatusOK)
	var rotated struct {
		Rotated   int  `json:"rotated"`
		NotStored bool `json:"new_key_hex_not_stored"`
	}
	b5trafficDecodeInto(t, rec, &rotated)
	if rotated.Rotated < len(refs) || !rotated.NotStored {
		t.Fatalf("轮换结果不符合预期: %+v", rotated)
	}
	for _, ref := range refs {
		if _, err := ev.GetBlob(context.Background(), ref); err != nil {
			t.Fatalf("轮换后同一 Store 读取失败：%v", err)
		}
	}
	newStore, err := evidence.New(evidence.Config{Dir: dir, Key: newKey})
	if err != nil {
		t.Fatalf("用新密钥重新打开失败: %v", err)
	}
	for _, ref := range refs {
		if _, err := newStore.GetBlob(context.Background(), ref); err != nil {
			t.Fatalf("新密钥读取失败：%v", err)
		}
	}
	oldStore, err := evidence.New(evidence.Config{Dir: dir, Key: b6Key(1)})
	if err != nil {
		t.Fatalf("用旧密钥重新打开失败: %v", err)
	}
	for _, ref := range refs {
		if _, err := oldStore.GetBlob(context.Background(), ref); !errors.Is(err, evidence.ErrTampered) {
			t.Fatalf("旧密钥应返回 ErrTampered，实际：%v", err)
		}
	}

	// Evidence 未装配：503。
	noEvidence := Options{Store: store, TenantID: tenantID}
	rec = invokeHandler(t, noEvidence.handleEvidenceRotate, http.MethodPost, "/api/v1/evidence/rotate", rotateBody, nil)
	requireStatus(t, rec, http.StatusServiceUnavailable)
}

// TestB6EvidenceAuditCoverage 验证 verify 与流量正文读取都会落审计。
func TestB6EvidenceAuditCoverage(t *testing.T) {
	store, ev, tenantID, _ := b6Setup(t)
	opts := Options{Store: store, Evidence: ev, TenantID: tenantID}
	mission := mustMission(t, store, tenantID, "审计覆盖任务")

	record := b5trafficCreateRecord(t, store, ev, tenantID, mission.ID, "http_probe",
		`{"url":"https://example.com"}`, "HTTP 200\n\nok", 200)
	reqRef := evidence.BlobRef{Hash: record.RequestHash, Size: record.RequestSize}
	respRef := evidence.BlobRef{Hash: record.ResponseHash, Size: record.ResponseSize}
	snapshot := b5trafficSnapshot(t, store, ev, tenantID, mission.ID, reqRef, respRef, "http_probe")
	snapshotPath := map[string]string{"id": jsonNumber(snapshot.ID)}

	// verify：成功路径记 verify。
	rec := invokeHandler(t, opts.handleVerifyEvidence, http.MethodGet,
		"/api/v1/evidence/"+snapshotPath["id"]+"/verify", "", snapshotPath)
	requireStatus(t, rec, http.StatusOK)
	// 读取流量请求正文：该 blob 同时被证据快照引用 → 记 read。
	rec = invokeHandler(t, opts.handleTrafficRequest, http.MethodGet,
		"/api/v1/traffic/"+jsonNumber(record.ID)+"/request", "", map[string]string{"id": jsonNumber(record.ID)})
	requireStatus(t, rec, http.StatusOK)

	rec = invokeHandler(t, opts.handleEvidenceAudit, http.MethodGet,
		"/api/v1/evidence/"+snapshotPath["id"]+"/audit", "", snapshotPath)
	requireStatus(t, rec, http.StatusOK)
	audit := decodeResponse[struct {
		Audit []pg.EvidenceAccessLog `json:"audit"`
	}](t, rec)
	actions := map[string]bool{}
	for _, entry := range audit.Audit {
		actions[entry.Action] = true
	}
	if !actions["verify"] {
		t.Errorf("缺少 verify 审计: %+v", audit.Audit)
	}
	if !actions["read"] {
		t.Errorf("缺少 read 审计: %+v", audit.Audit)
	}
}

// TestB6FindingNodeUnique 验证同一节点只能关联一条发现（DB 唯一索引 + 409 语义）。
func TestB6FindingNodeUnique(t *testing.T) {
	store, ev, tenantID, _ := b6Setup(t)
	opts := Options{Store: store, Evidence: ev, TenantID: tenantID}
	mission := mustMission(t, store, tenantID, "节点唯一任务")
	ctx := context.Background()

	node, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateOpen, Title: "SQL 注入事实", Body: "id 可注入",
	})
	if err != nil {
		t.Fatalf("创建事实节点失败: %v", err)
	}
	nodeID := node.ID
	if _, err := store.CreateFinding(ctx, tenantID, mission.ID, pg.FindingInput{
		NodeID: &nodeID, Title: "发现一", Severity: "high",
	}); err != nil {
		t.Fatalf("首次创建发现失败: %v", err)
	}
	// 直接走 Store 的第二次创建：由 0016 唯一索引兜底，映射为 ErrConflict。
	if _, err := store.CreateFinding(ctx, tenantID, mission.ID, pg.FindingInput{
		NodeID: &nodeID, Title: "发现二", Severity: "low",
	}); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("重复节点创建应返回 ErrConflict，实际: %v", err)
	}

	// HTTP from-node：第二次 409。
	missionPath := map[string]string{"id": jsonNumber(mission.ID)}
	other, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateOpen, Title: "第二个事实",
	})
	if err != nil {
		t.Fatalf("创建事实节点失败: %v", err)
	}
	body := `{"node_id":` + jsonNumber(other.ID) + `}`
	rec := invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node", body, missionPath)
	requireStatus(t, rec, http.StatusCreated)
	rec = invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node", body, missionPath)
	requireStatus(t, rec, http.StatusConflict)
	requireErrorCode(t, rec, "conflict")

	// 并发兜底：唯一索引存在时第二次直接写入必须失败（而不是插入两条）。
	var nodeCount int
	if err := findingsHTTPDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM findings WHERE tenant_id=$1 AND node_id=$2`, tenantID, nodeID).Scan(&nodeCount); err != nil {
		t.Fatalf("统计节点发现失败: %v", err)
	}
	if nodeCount != 1 {
		t.Fatalf("同一节点发现数 = %d，期望 1", nodeCount)
	}
}

// jsonNumber 把 int64 转为十进制字符串（与 b5 测试内联 fmt.Sprint 保持同样的可读性）。
func jsonNumber(v int64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
