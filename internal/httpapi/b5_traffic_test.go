package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/plan"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B5c：报告导出 / 流量导出重放 / 证据审计导出 / 复测执行 / AI finding 集成测试。
//
// 依赖真实 PostgreSQL + 临时证据目录（t.TempDir）；未设置 BEACONX_TEST_PG_DSN
// 时跳过。helper 统一 b5traffic 前缀，避免与同包其它 B5 批次测试重名。

// b5trafficSetup 准备 Store + Evidence + 独立租户。
func b5trafficSetup(t *testing.T) (*pg.Store, *evidence.Store, int64) {
	t.Helper()
	store, tenantID := findingsHTTPEnv(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	ev, err := evidence.New(evidence.Config{Dir: t.TempDir(), Key: key})
	if err != nil {
		t.Fatalf("创建证据存储失败: %v", err)
	}
	return store, ev, tenantID
}

func b5trafficOptions(store *pg.Store, ev *evidence.Store, tenantID int64) Options {
	return Options{Store: store, Evidence: ev, TenantID: tenantID}
}

// b5trafficRef 把一段正文写入证据 blob。
func b5trafficRef(t *testing.T, ev *evidence.Store, data string) evidence.BlobRef {
	t.Helper()
	ref, err := ev.PutBlob(context.Background(), []byte(data))
	if err != nil {
		t.Fatalf("写入证据 blob 失败: %v", err)
	}
	return ref
}

// b5trafficCreateRecord 创建一条已完成的采集记录（intent → result）。
func b5trafficCreateRecord(t *testing.T, store *pg.Store, ev *evidence.Store, tenantID, missionID int64,
	tool, args, output string, status int) pg.TrafficExchange {
	t.Helper()
	req := b5trafficRef(t, ev, args)
	resp := b5trafficRef(t, ev, output)
	id, err := store.BeginTraffic(context.Background(), tenantID, missionID, nil, tool, req, "json-keys")
	if err != nil {
		t.Fatalf("写入流量请求失败: %v", err)
	}
	if err := store.FinishTraffic(context.Background(), tenantID, id, resp, status, ""); err != nil {
		t.Fatalf("写入流量响应失败: %v", err)
	}
	record, err := store.GetTraffic(context.Background(), tenantID, id)
	if err != nil {
		t.Fatalf("读取流量记录失败: %v", err)
	}
	return record
}

// b5trafficCreateRawRecord 直接写入指定 BlobRef 的采集记录（用于构造缺失/超大正文）。
func b5trafficCreateRawRecord(t *testing.T, store *pg.Store, tenantID, missionID int64,
	tool string, req, resp evidence.BlobRef) pg.TrafficExchange {
	t.Helper()
	id, err := store.BeginTraffic(context.Background(), tenantID, missionID, nil, tool, req, "json-keys")
	if err != nil {
		t.Fatalf("写入流量请求失败: %v", err)
	}
	if err := store.FinishTraffic(context.Background(), tenantID, id, resp, 200, ""); err != nil {
		t.Fatalf("写入流量响应失败: %v", err)
	}
	record, err := store.GetTraffic(context.Background(), tenantID, id)
	if err != nil {
		t.Fatalf("读取流量记录失败: %v", err)
	}
	return record
}

// b5trafficSnapshot 冻结一条证据快照（签名与 created_at 与 B3 接口一致）。
func b5trafficSnapshot(t *testing.T, store *pg.Store, ev *evidence.Store, tenantID, missionID int64,
	req, resp evidence.BlobRef, tool string) pg.EvidenceSnapshot {
	t.Helper()
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	sealed, err := ev.Seal(evidence.Snapshot{
		MissionID: missionID,
		Tool:      tool,
		Request:   req,
		Response:  resp,
		Redaction: "json-keys",
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("Seal 证据失败: %v", err)
	}
	snapshot, err := store.CreateEvidenceSnapshot(context.Background(), tenantID, missionID,
		tool, req, resp, sealed.Signature, "json-keys", createdAt)
	if err != nil {
		t.Fatalf("创建证据快照失败: %v", err)
	}
	return snapshot
}

// b5trafficHAR 是用于断言的最小 HAR 结构。
type b5trafficHAR struct {
	Log struct {
		Version string `json:"version"`
		Entries []struct {
			StartedDateTime string `json:"startedDateTime"`
			Request         struct {
				Method   string                         `json:"method"`
				URL      string                         `json:"url"`
				Headers  []struct{ Name, Value string } `json:"headers"`
				PostData *struct{ Text string }         `json:"postData"`
			} `json:"request"`
			Response struct {
				Status  int `json:"status"`
				Content struct {
					MimeType string `json:"mimeType"`
					Text     string `json:"text"`
				} `json:"content"`
			} `json:"response"`
		} `json:"entries"`
	} `json:"log"`
}

// TestB5TrafficHARAndCleanup 覆盖 HAR 导出、错误语义与任务级流量清理。
func TestB5TrafficHARAndCleanup(t *testing.T) {
	store, ev, tenantID := b5trafficSetup(t)
	opts := b5trafficOptions(store, ev, tenantID)
	mission := mustMission(t, store, tenantID, "HAR 导出任务")

	args := `{"url":"https://example.com/api?x=1","method":"GET","headers":{"X-Test":"yes"}}`
	output := "HTTP 200\nContent-Type: application/json\nServer: nginx\n\n{\"ok\":true}"
	record := b5trafficCreateRecord(t, store, ev, tenantID, mission.ID, "http_probe", args, output, 200)
	recordPath := map[string]string{"id": fmt.Sprint(record.ID)}

	rec := invokeHandler(t, opts.handleTrafficExportHAR, http.MethodGet,
		"/api/v1/traffic/"+recordPath["id"]+"/export?format=har", "", recordPath)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q，期望 application/json", ct)
	}
	if disposition := rec.Header().Get("Content-Disposition"); !strings.Contains(disposition, fmt.Sprintf("traffic-%d.har", record.ID)) {
		t.Errorf("Content-Disposition = %q", disposition)
	}
	var har b5trafficHAR
	if err := json.Unmarshal(rec.Body.Bytes(), &har); err != nil {
		t.Fatalf("HAR 不是合法 JSON：%v", err)
	}
	if har.Log.Version != "1.2" || len(har.Log.Entries) != 1 {
		t.Fatalf("HAR 结构不符合预期: %+v", har.Log)
	}
	entry := har.Log.Entries[0]
	if entry.Request.Method != "GET" || !strings.Contains(entry.Request.URL, "example.com/api") {
		t.Errorf("HAR request = %+v", entry.Request)
	}
	if len(entry.Request.Headers) != 1 || entry.Request.Headers[0].Name != "X-Test" {
		t.Errorf("HAR request headers = %+v", entry.Request.Headers)
	}
	if entry.Response.Status != 200 || !strings.Contains(entry.Response.Content.Text, `{"ok":true}`) {
		t.Errorf("HAR response = %+v", entry.Response)
	}
	if entry.Response.Content.MimeType != "application/json" {
		t.Errorf("HAR content mimeType = %q", entry.Response.Content.MimeType)
	}

	// fail_unsupported_format：非 har → 400。
	rec = invokeHandler(t, opts.handleTrafficExportHAR, http.MethodGet,
		"/api/v1/traffic/"+recordPath["id"]+"/export?format=pcap", "", recordPath)
	requireStatus(t, rec, http.StatusBadRequest)
	// 不存在的记录 → 404。
	rec = invokeHandler(t, opts.handleTrafficExportHAR, http.MethodGet,
		"/api/v1/traffic/999999999/export?format=har", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
	// 正文 blob 缺失/被篡改 → 409。
	missing := b5trafficCreateRawRecord(t, store, tenantID, mission.ID, "http_probe",
		evidence.BlobRef{Hash: strings.Repeat("d", 64), Size: 10},
		b5trafficRef(t, ev, "resp"))
	rec = invokeHandler(t, opts.handleTrafficExportHAR, http.MethodGet,
		fmt.Sprintf("/api/v1/traffic/%d/export?format=har", missing.ID), "", map[string]string{"id": fmt.Sprint(missing.ID)})
	requireStatus(t, rec, http.StatusConflict)
	requireErrorCode(t, rec, "tampered")
	// 超过 20MB → 413（不读取 blob）。
	oversize := b5trafficCreateRawRecord(t, store, tenantID, mission.ID, "http_probe",
		evidence.BlobRef{Hash: strings.Repeat("e", 64), Size: 21 << 20}, evidence.BlobRef{})
	rec = invokeHandler(t, opts.handleTrafficExportHAR, http.MethodGet,
		fmt.Sprintf("/api/v1/traffic/%d/export?format=har", oversize.ID), "", map[string]string{"id": fmt.Sprint(oversize.ID)})
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)

	// 任务级清理：删除全部采集行，证据快照保留。
	reqRef := evidence.BlobRef{Hash: record.RequestHash, Size: record.RequestSize}
	respRef := evidence.BlobRef{Hash: record.ResponseHash, Size: record.ResponseSize}
	snapshot := b5trafficSnapshot(t, store, ev, tenantID, mission.ID, reqRef, respRef, "http_probe")
	rec = invokeHandler(t, opts.handleDeleteMissionTraffic, http.MethodDelete,
		"/api/v1/missions/"+fmt.Sprint(mission.ID)+"/traffic", "", map[string]string{"id": fmt.Sprint(mission.ID)})
	requireStatus(t, rec, http.StatusNoContent)
	if got := rec.Header().Get("X-Deleted-Count"); got == "" || got == "0" {
		t.Errorf("X-Deleted-Count = %q，期望 >0", got)
	}
	list, err := store.ListTraffic(context.Background(), tenantID, mission.ID, 100)
	if err != nil {
		t.Fatalf("读取任务流量失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("清理后仍有 %d 条流量记录", len(list))
	}
	if _, err := store.GetEvidenceSnapshot(context.Background(), tenantID, snapshot.ID); err != nil {
		t.Fatalf("流量清理不应删除证据快照：%v", err)
	}

	// 跨租户：另一个租户看不到该流量记录。
	otherTenant := mustExtraTenant(t)
	otherOpts := b5trafficOptions(store, ev, otherTenant)
	rec = invokeHandler(t, otherOpts.handleTrafficExportHAR, http.MethodGet,
		"/api/v1/traffic/"+recordPath["id"]+"/export?format=har", "", recordPath)
	requireStatus(t, rec, http.StatusNotFound)
}

// TestB5TrafficReplay 覆盖 http_probe 重放的成功与不可重放分支。
func TestB5TrafficReplay(t *testing.T) {
	store, ev, tenantID := b5trafficSetup(t)
	opts := b5trafficOptions(store, ev, tenantID)
	mission := mustMission(t, store, tenantID, "重放任务")

	received := make(chan *http.Request, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Clone(context.Background())
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "created")
	}))
	defer target.Close()

	args := fmt.Sprintf(`{"url":%q,"method":"POST","headers":{"X-Replay":"1"},"body":"hi"}`, target.URL)
	record := b5trafficCreateRecord(t, store, ev, tenantID, mission.ID, "http_probe", args, "HTTP 200\n\nok", 200)
	recordPath := map[string]string{"id": fmt.Sprint(record.ID)}

	rec := invokeHandler(t, opts.handleTrafficReplay, http.MethodPost,
		"/api/v1/traffic/"+recordPath["id"]+"/replay", "", recordPath)
	requireStatus(t, rec, http.StatusOK)
	var replay struct {
		TrafficID int64  `json:"traffic_id"`
		MissionID int64  `json:"mission_id"`
		Status    int    `json:"status"`
		Error     string `json:"error"`
	}
	b5trafficDecodeInto(t, rec, &replay)
	if replay.TrafficID == 0 || replay.TrafficID == record.ID {
		t.Fatalf("重放未生成新的 traffic id: %+v", replay)
	}
	if replay.Status != http.StatusCreated || replay.MissionID != mission.ID {
		t.Fatalf("重放结果不符合预期: %+v", replay)
	}
	select {
	case request := <-received:
		if request.Method != http.MethodPost || request.Header.Get("X-Replay") != "1" {
			t.Errorf("重放请求 = %s %v", request.Method, request.Header)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("重放请求未到达目标")
	}
	newRecord, err := store.GetTraffic(context.Background(), tenantID, replay.TrafficID)
	if err != nil {
		t.Fatalf("读取重放采集记录失败: %v", err)
	}
	if newRecord.Tool != "http_probe" || newRecord.Phase != "result" || newRecord.ResponseHash == "" {
		t.Fatalf("重放采集记录不符合预期: %+v", newRecord)
	}

	// 非 http_probe → 409。
	other := b5trafficCreateRecord(t, store, ev, tenantID, mission.ID, "shell_exec", `{"cmd":"id"}`, "uid=0", 0)
	rec = invokeHandler(t, opts.handleTrafficReplay, http.MethodPost,
		fmt.Sprintf("/api/v1/traffic/%d/replay", other.ID), "", map[string]string{"id": fmt.Sprint(other.ID)})
	requireStatus(t, rec, http.StatusConflict)

	// 没有请求正文 → 409。
	emptyReq := b5trafficCreateRawRecord(t, store, tenantID, mission.ID, "http_probe",
		evidence.BlobRef{}, b5trafficRef(t, ev, "x"))
	rec = invokeHandler(t, opts.handleTrafficReplay, http.MethodPost,
		fmt.Sprintf("/api/v1/traffic/%d/replay", emptyReq.ID), "", map[string]string{"id": fmt.Sprint(emptyReq.ID)})
	requireStatus(t, rec, http.StatusConflict)

	// 不存在 → 404；Evidence 未装配 → 503。
	rec = invokeHandler(t, opts.handleTrafficReplay, http.MethodPost,
		"/api/v1/traffic/999999999/replay", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
	noEvidence := Options{Store: store, TenantID: tenantID}
	rec = invokeHandler(t, noEvidence.handleTrafficReplay, http.MethodPost,
		"/api/v1/traffic/"+recordPath["id"]+"/replay", "", recordPath)
	requireStatus(t, rec, http.StatusServiceUnavailable)
}

// decodeResponseInto 解码 JSON 响应到指定指针（用于匿名结构体）。
func b5trafficDecodeInto(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("解析响应失败: %v，body=%s", err, rec.Body.String())
	}
}

// TestB5EvidenceAuditExportRotate 覆盖证据审计、导出 zip 与密钥轮换 501。
func TestB5EvidenceAuditExportRotate(t *testing.T) {
	store, ev, tenantID := b5trafficSetup(t)
	opts := b5trafficOptions(store, ev, tenantID)
	mission := mustMission(t, store, tenantID, "证据导出任务")

	req := b5trafficRef(t, ev, `{"url":"https://example.com"}`)
	resp := b5trafficRef(t, ev, "HTTP 200\n\nok")
	snapshot := b5trafficSnapshot(t, store, ev, tenantID, mission.ID, req, resp, "http_probe")
	snapshotPath := map[string]string{"id": fmt.Sprint(snapshot.ID)}

	// 还没发生访问时：审计是空数组而不是 null。
	rec := invokeHandler(t, opts.handleEvidenceAudit, http.MethodGet,
		"/api/v1/evidence/"+snapshotPath["id"]+"/audit", "", snapshotPath)
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"audit":[]`) {
		t.Errorf("空审计列表应输出 []，实际: %s", rec.Body.String())
	}

	// 导出 zip：snapshot.json + 请求/响应正文。
	rec = invokeHandler(t, opts.handleEvidenceExport, http.MethodPost,
		"/api/v1/evidence/"+snapshotPath["id"]+"/export", "", snapshotPath)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q，期望 application/zip", ct)
	}
	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("证据包不是合法 zip：%v", err)
	}
	files := map[string][]byte{}
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("打开 zip 条目 %s 失败: %v", file.Name, err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		files[file.Name] = body
	}
	if string(files["request.bin"]) != `{"url":"https://example.com"}` {
		t.Errorf("request.bin = %q", files["request.bin"])
	}
	if string(files["response.bin"]) != "HTTP 200\n\nok" {
		t.Errorf("response.bin = %q", files["response.bin"])
	}
	if !bytes.Contains(files["snapshot.json"], []byte(snapshot.Signature)) {
		t.Errorf("snapshot.json 缺少签名: %s", files["snapshot.json"])
	}

	// 审计：应有一条 export 记录。
	rec = invokeHandler(t, opts.handleEvidenceAudit, http.MethodGet,
		"/api/v1/evidence/"+snapshotPath["id"]+"/audit", "", snapshotPath)
	requireStatus(t, rec, http.StatusOK)
	audit := decodeResponse[struct {
		Audit []pg.EvidenceAccessLog `json:"audit"`
	}](t, rec)
	if len(audit.Audit) != 1 || audit.Audit[0].Action != "export" || audit.Audit[0].SnapshotID != snapshot.ID {
		t.Fatalf("证据审计不符合预期: %+v", audit.Audit)
	}

	// 轮换：明确 501，不产生半完成状态。
	rec = invokeHandler(t, opts.handleEvidenceRotate, http.MethodPost,
		"/api/v1/evidence/rotate", `{"new_key_hex":"00"}`, nil)
	requireStatus(t, rec, http.StatusNotImplemented)
	requireErrorCode(t, rec, "not_implemented")

	// 不存在 / 非法 id / 跨租户。
	rec = invokeHandler(t, opts.handleEvidenceAudit, http.MethodGet,
		"/api/v1/evidence/999999999/audit", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
	rec = invokeHandler(t, opts.handleEvidenceExport, http.MethodPost,
		"/api/v1/evidence/not-a-number/export", "", map[string]string{"id": "not-a-number"})
	requireStatus(t, rec, http.StatusBadRequest)
	otherTenant := mustExtraTenant(t)
	otherOpts := b5trafficOptions(store, ev, otherTenant)
	rec = invokeHandler(t, otherOpts.handleEvidenceExport, http.MethodPost,
		"/api/v1/evidence/"+snapshotPath["id"]+"/export", "", snapshotPath)
	requireStatus(t, rec, http.StatusNotFound)
}

// TestB5RetestRunAndEvidence 覆盖复测执行三态与关联证据列表。
func TestB5RetestRunAndEvidence(t *testing.T) {
	store, ev, tenantID := b5trafficSetup(t)
	opts := b5trafficOptions(store, ev, tenantID)
	mission := mustMission(t, store, tenantID, "复测执行任务")
	ctx := context.Background()

	// 有完整证据：passed。
	finding := mustCreateFindingWith(t, store, tenantID, mission.ID, "SQL 注入")
	req := b5trafficRef(t, ev, `{"url":"https://example.com"}`)
	resp := b5trafficRef(t, ev, "HTTP 200\n\nok")
	snapshot := b5trafficSnapshot(t, store, ev, tenantID, mission.ID, req, resp, "http_probe")
	if err := store.AddFindingEvidence(ctx, tenantID, finding.ID, snapshot.ID); err != nil {
		t.Fatalf("绑定证据失败: %v", err)
	}
	retest, err := store.CreateRetest(ctx, tenantID, mission.ID, finding.ID, "复核", "user")
	if err != nil {
		t.Fatalf("创建复测失败: %v", err)
	}
	retestPath := map[string]string{"id": fmt.Sprint(retest.ID)}

	rec := invokeHandler(t, opts.handleRetestEvidence, http.MethodGet,
		"/api/v1/retests/"+retestPath["id"]+"/evidence", "", retestPath)
	requireStatus(t, rec, http.StatusOK)
	evidenceList := decodeResponse[struct {
		Evidence []pg.EvidenceSnapshot `json:"evidence"`
	}](t, rec)
	if len(evidenceList.Evidence) != 1 || evidenceList.Evidence[0].ID != snapshot.ID {
		t.Fatalf("复测证据列表不符合预期: %+v", evidenceList.Evidence)
	}

	rec = invokeHandler(t, opts.handleRetestRun, http.MethodPost,
		"/api/v1/retests/"+retestPath["id"]+"/run", "", retestPath)
	requireStatus(t, rec, http.StatusOK)
	passed := decodeResponse[pg.Retest](t, rec)
	if passed.Status != "passed" || passed.StartedAt == nil || passed.FinishedAt == nil {
		t.Fatalf("复测通过结果不符合预期: %+v", passed)
	}
	// 已终态：再跑 409。
	rec = invokeHandler(t, opts.handleRetestRun, http.MethodPost,
		"/api/v1/retests/"+retestPath["id"]+"/run", "", retestPath)
	requireStatus(t, rec, http.StatusConflict)

	// 无证据：error。
	emptyFinding := mustCreateFindingWith(t, store, tenantID, mission.ID, "无证据发现")
	emptyRetest, err := store.CreateRetest(ctx, tenantID, mission.ID, emptyFinding.ID, "", "user")
	if err != nil {
		t.Fatalf("创建复测失败: %v", err)
	}
	rec = invokeHandler(t, opts.handleRetestRun, http.MethodPost,
		fmt.Sprintf("/api/v1/retests/%d/run", emptyRetest.ID), "", map[string]string{"id": fmt.Sprint(emptyRetest.ID)})
	requireStatus(t, rec, http.StatusOK)
	noEvidenceStatus := decodeResponse[pg.Retest](t, rec)
	if noEvidenceStatus.Status != "error" || !strings.Contains(noEvidenceStatus.Note, "无证据快照") {
		t.Fatalf("无证据复测结果不符合预期: %+v", noEvidenceStatus)
	}
	rec = invokeHandler(t, opts.handleRetestEvidence, http.MethodGet,
		fmt.Sprintf("/api/v1/retests/%d/evidence", emptyRetest.ID), "", map[string]string{"id": fmt.Sprint(emptyRetest.ID)})
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"evidence":[]`) {
		t.Errorf("无证据复测的证据列表应输出 []，实际: %s", rec.Body.String())
	}

	// 签名被篡改：failed。
	tamperedFinding := mustCreateFindingWith(t, store, tenantID, mission.ID, "被篡改证据")
	tampered := b5trafficSnapshot(t, store, ev, tenantID, mission.ID, req, resp, "http_probe")
	if err := store.AddFindingEvidence(ctx, tenantID, tamperedFinding.ID, tampered.ID); err != nil {
		t.Fatalf("绑定证据失败: %v", err)
	}
	if _, err := findingsHTTPDB.Pool().Exec(ctx,
		`UPDATE evidence_snapshots SET signature=$2 WHERE tenant_id=$1 AND id=$3`,
		tenantID, strings.Repeat("0", 64), tampered.ID); err != nil {
		t.Fatalf("篡改签名失败: %v", err)
	}
	tamperedRetest, err := store.CreateRetest(ctx, tenantID, mission.ID, tamperedFinding.ID, "", "user")
	if err != nil {
		t.Fatalf("创建复测失败: %v", err)
	}
	rec = invokeHandler(t, opts.handleRetestRun, http.MethodPost,
		fmt.Sprintf("/api/v1/retests/%d/run", tamperedRetest.ID), "", map[string]string{"id": fmt.Sprint(tamperedRetest.ID)})
	requireStatus(t, rec, http.StatusOK)
	failed := decodeResponse[pg.Retest](t, rec)
	if failed.Status != "failed" || !strings.Contains(failed.Note, "签名") {
		t.Fatalf("篡改证据复测结果不符合预期: %+v", failed)
	}

	// 不存在 / 跨租户。
	rec = invokeHandler(t, opts.handleRetestRun, http.MethodPost,
		"/api/v1/retests/999999999/run", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
	otherTenant := mustExtraTenant(t)
	otherOpts := b5trafficOptions(store, ev, otherTenant)
	rec = invokeHandler(t, otherOpts.handleRetestEvidence, http.MethodGet,
		"/api/v1/retests/"+retestPath["id"]+"/evidence", "", retestPath)
	requireStatus(t, rec, http.StatusNotFound)
}

// TestB5FindingFromNode 覆盖从 fact/finding 节点创建发现的各分支。
func TestB5FindingFromNode(t *testing.T) {
	store, ev, tenantID := b5trafficSetup(t)
	opts := b5trafficOptions(store, ev, tenantID)
	mission := mustMission(t, store, tenantID, "AI finding 任务")
	missionPath := map[string]string{"id": fmt.Sprint(mission.ID)}
	ctx := context.Background()

	fact, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateOpen, Title: "发现 SQL 注入", Body: "id 参数可注入",
	})
	if err != nil {
		t.Fatalf("创建事实节点失败: %v", err)
	}
	goal, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeGoal, State: graph.StateOpen, Title: "目标节点", Body: "",
	})
	if err != nil {
		t.Fatalf("创建目标节点失败: %v", err)
	}

	// 成功：201，node_id/detail 来自节点。
	rec := invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node",
		fmt.Sprintf(`{"node_id":%d,"title":"SQL 注入","severity":"high"}`, fact.ID), missionPath)
	requireStatus(t, rec, http.StatusCreated)
	created := decodeResponse[pg.Finding](t, rec)
	if created.NodeID == nil || *created.NodeID != fact.ID || created.Title != "SQL 注入" ||
		created.Severity != "high" || created.Detail != "id 参数可注入" {
		t.Fatalf("从节点创建发现结果不符合预期: %+v", created)
	}
	// 重复：409。
	rec = invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node",
		fmt.Sprintf(`{"node_id":%d}`, fact.ID), missionPath)
	requireStatus(t, rec, http.StatusConflict)
	// 非 fact/finding：400。
	rec = invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node",
		fmt.Sprintf(`{"node_id":%d}`, goal.ID), missionPath)
	requireStatus(t, rec, http.StatusBadRequest)
	// 不存在节点：404；非法 severity：400。
	rec = invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node", `{"node_id":999999999}`, missionPath)
	requireStatus(t, rec, http.StatusNotFound)
	fact2, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateOpen, Title: "第二个事实", Body: "",
	})
	if err != nil {
		t.Fatalf("创建事实节点失败: %v", err)
	}
	rec = invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node",
		fmt.Sprintf(`{"node_id":%d,"severity":"urgent"}`, fact2.ID), missionPath)
	requireStatus(t, rec, http.StatusBadRequest)

	// 缺省标题/严重级别：从节点回填。
	rec = invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node",
		fmt.Sprintf(`{"node_id":%d}`, fact2.ID), missionPath)
	requireStatus(t, rec, http.StatusCreated)
	defaulted := decodeResponse[pg.Finding](t, rec)
	if defaulted.Title != "第二个事实" || defaulted.Severity != "info" {
		t.Fatalf("缺省字段回填不符合预期: %+v", defaulted)
	}

	// 跨任务节点：404。
	otherMission := mustMission(t, store, tenantID, "另一个任务")
	otherNode, err := store.AddNode(ctx, tenantID, otherMission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateOpen, Title: "其它任务事实",
	})
	if err != nil {
		t.Fatalf("创建跨任务节点失败: %v", err)
	}
	rec = invokeHandler(t, opts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node",
		fmt.Sprintf(`{"node_id":%d}`, otherNode.ID), missionPath)
	requireStatus(t, rec, http.StatusNotFound)

	// 跨租户访问任务：404。
	otherTenant := mustExtraTenant(t)
	otherOpts := b5trafficOptions(store, ev, otherTenant)
	rec = invokeHandler(t, otherOpts.handleFindingFromNode, http.MethodPost,
		"/api/v1/missions/"+missionPath["id"]+"/findings/from-node",
		fmt.Sprintf(`{"node_id":%d}`, fact.ID), missionPath)
	requireStatus(t, rec, http.StatusNotFound)
}

// TestB5ReportFormats 覆盖 markdown/html/docx/pdf 四种报告导出格式。
func TestB5ReportFormats(t *testing.T) {
	store, ev, tenantID := b5trafficSetup(t)
	opts := b5trafficOptions(store, ev, tenantID)
	mission := mustMission(t, store, tenantID, "导出报告任务")
	ctx := context.Background()
	missionPath := map[string]string{"id": fmt.Sprint(mission.ID)}

	if _, err := store.UpsertPlanSteps(ctx, tenantID, mission.ID, []plan.Step{
		{Key: "recon", Title: "信息收集"},
	}); err != nil {
		t.Fatalf("写入计划步骤失败: %v", err)
	}
	if _, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateOpen, Title: "目标使用 nginx", Body: "Server: nginx/1.25",
	}); err != nil {
		t.Fatalf("创建事实节点失败: %v", err)
	}
	if _, err := store.CreateFinding(ctx, tenantID, mission.ID, pg.FindingInput{
		Title: "SQL 注入", Severity: "critical",
	}); err != nil {
		t.Fatalf("创建发现失败: %v", err)
	}
	reportURL := "/api/v1/missions/" + missionPath["id"] + "/report"

	// html。
	rec := invokeHandler(t, opts.handleMissionReport, http.MethodGet, reportURL+"?format=html", "", missionPath)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("HTML Content-Type = %q", ct)
	}
	html := rec.Body.String()
	if !strings.Contains(html, "<h1>任务报告：导出报告任务</h1>") || !strings.Contains(html, "SQL 注入") {
		t.Errorf("HTML 报告缺少关键内容")
	}

	// docx：zip + document.xml。
	rec = invokeHandler(t, opts.handleMissionReport, http.MethodGet, reportURL+"?format=docx", "", missionPath)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "wordprocessingml.document") {
		t.Errorf("DOCX Content-Type = %q", ct)
	}
	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("DOCX 不是合法 zip：%v", err)
	}
	var documentXML string
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		rc, _ := file.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		documentXML = string(body)
	}
	if !strings.Contains(documentXML, "导出报告任务") || !strings.Contains(documentXML, "SQL 注入") {
		t.Error("DOCX document.xml 缺少报告内容")
	}

	// pdf：以 %PDF- 开头、以 %%EOF 结尾，含 CJK 字体引用。
	rec = invokeHandler(t, opts.handleMissionReport, http.MethodGet, reportURL+"?format=pdf", "", missionPath)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("PDF Content-Type = %q", ct)
	}
	pdf := rec.Body.Bytes()
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) {
		t.Errorf("PDF 头不合法: %q", pdf[:8])
	}
	if !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Error("PDF 缺少 EOF 结束标记")
	}
	if !bytes.Contains(pdf, []byte("/STSong-Light")) {
		t.Error("PDF 缺少 CJK 字体引用")
	}

	// markdown 缺省与显式 format=markdown 都保持原行为。
	for _, url := range []string{reportURL, reportURL + "?format=markdown"} {
		rec = invokeHandler(t, opts.handleMissionReport, http.MethodGet, url, "", missionPath)
		requireStatus(t, rec, http.StatusOK)
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
			t.Errorf("Markdown Content-Type = %q", ct)
		}
		if !strings.Contains(rec.Body.String(), "# 任务报告：") {
			t.Error("Markdown 报告缺少标题")
		}
	}

	// 非法 format → 400；任务不存在 → 404；跨租户 → 404。
	rec = invokeHandler(t, opts.handleMissionReport, http.MethodGet, reportURL+"?format=xlsx", "", missionPath)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleMissionReport, http.MethodGet,
		"/api/v1/missions/999999999/report?format=pdf", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
	otherTenant := mustExtraTenant(t)
	otherOpts := b5trafficOptions(store, ev, otherTenant)
	rec = invokeHandler(t, otherOpts.handleMissionReport, http.MethodGet, reportURL+"?format=html", "", missionPath)
	requireStatus(t, rec, http.StatusNotFound)
}
