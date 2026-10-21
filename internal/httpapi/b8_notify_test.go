package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B8 清理测试：通知配置加密、事件推送 dispatcher、审计埋点、全租户重签、启动维护。

// TestB8EvidenceSecretRoundTrip 覆盖通知子密钥加密/解密与错误密钥。
func TestB8EvidenceSecretRoundTrip(t *testing.T) {
	_, _, dir := b8Setup(t)
	token, err := b8Evidence(t, dir).EncryptSecret("s3cr3t-值")
	if err != nil {
		t.Fatalf("EncryptSecret：%v", err)
	}
	if token == "" || strings.Contains(token, "s3cr3t-值") {
		t.Fatalf("密文格式异常: %q", token)
	}
	opened, err := b8Evidence(t, dir).DecryptSecret(token)
	if err != nil || opened != "s3cr3t-值" {
		t.Fatalf("DecryptSecret = %q, %v", opened, err)
	}
	// 错误密钥无法解密。
	wrong, err := evidence.New(evidence.Config{Dir: dir, Key: b6Key(99)})
	if err != nil {
		t.Fatalf("创建错误密钥存储失败: %v", err)
	}
	if _, err := wrong.DecryptSecret(token); err == nil {
		t.Fatal("错误密钥解密应失败")
	}
	// 空串原样往返。
	if empty, err := b8Evidence(t, dir).EncryptSecret(""); err != nil || empty != "" {
		t.Fatalf("空串加密 = %q, %v", empty, err)
	}
}

// b8Evidence 用同一目录/密钥打开证据存储（模拟进程重启后的句柄）。
func b8Evidence(t *testing.T, dir string) *evidence.Store {
	t.Helper()
	ev, err := evidence.New(evidence.Config{Dir: dir, Key: b6Key(1)})
	if err != nil {
		t.Fatalf("打开证据存储失败: %v", err)
	}
	return ev
}

// b8Setup 准备 Store + Evidence + 租户 + 证据目录。
func b8Setup(t *testing.T) (*pg.Store, int64, string) {
	t.Helper()
	store, tenantID := findingsHTTPEnv(t)
	dir := t.TempDir()
	return store, tenantID, dir
}

// b8Options 构造带 Evidence 的 Options。
func b8Options(t *testing.T, store *pg.Store, tenantID int64, dir string) Options {
	t.Helper()
	return Options{Store: store, Evidence: b8Evidence(t, dir), TenantID: tenantID}
}

// TestB8NotificationConfigEncryption 覆盖敏感配置加密存储、响应掩码、旧明文迁移与 503。
func TestB8NotificationConfigEncryption(t *testing.T) {
	store, tenantID, dir := b8Setup(t)
	opts := b8Options(t, store, tenantID, dir)

	body := `{"name":"加密渠道","type":"webhook","config":{
		"url":"http://user:pass@example.com/hook",
		"token":"plain-token-1",
		"nested":{"api_key":"nested-secret","plain":"ok"},
		"plain":"visible"
	},"enabled":true}`
	rec := invokeHandler(t, opts.handleCreateNotificationChannel, http.MethodPost,
		"/api/v1/notifications/channels", body, nil)
	requireStatus(t, rec, http.StatusCreated)
	created := decodeResponse[pg.NotificationChannel](t, rec)
	// 响应仍按 B7 语义掩码。
	if created.Config["token"] != "***" {
		t.Errorf("响应 token 未掩码: %v", created.Config["token"])
	}
	if urlText, _ := created.Config["url"].(string); strings.Contains(urlText, "user:pass") {
		t.Errorf("响应 URL 泄漏 userinfo: %q", urlText)
	}

	// DB 里不能出现明文敏感值；存储态带 __enc 标记。
	var storedConfig string
	if err := findingsHTTPDB.Pool().QueryRow(context.Background(),
		`SELECT config::text FROM notification_channels WHERE tenant_id=$1 AND id=$2`,
		tenantID, created.ID).Scan(&storedConfig); err != nil {
		t.Fatalf("读取存储配置失败: %v", err)
	}
	for _, plaintext := range []string{"plain-token-1", "nested-secret", "user:pass"} {
		if strings.Contains(storedConfig, plaintext) {
			t.Fatalf("DB 中残留明文 %q: %s", plaintext, storedConfig)
		}
	}
	if !strings.Contains(storedConfig, `"__enc": true`) && !strings.Contains(storedConfig, `"__enc":true`) {
		t.Fatalf("存储配置缺少加密标记: %s", storedConfig)
	}

	// 详情解密后掩码，非敏感值保留。
	path := map[string]string{"id": fmt.Sprint(created.ID)}
	rec = invokeHandler(t, opts.handleGetNotificationChannel, http.MethodGet,
		"/api/v1/notifications/channels/"+path["id"], "", path)
	requireStatus(t, rec, http.StatusOK)
	got := decodeResponse[pg.NotificationChannel](t, rec)
	if got.Config["plain"] != "visible" || got.Config["token"] != "***" {
		t.Fatalf("详情脱敏不符合预期: %+v", got.Config)
	}

	// PATCH 新配置：旧密文被替换，DB 不含新旧明文。
	rec = invokeHandler(t, opts.handleUpdateNotificationChannel, http.MethodPatch,
		"/api/v1/notifications/channels/"+path["id"],
		`{"config":{"url":"http://example.com/hook","token":"plain-token-2"}}`, path)
	requireStatus(t, rec, http.StatusOK)
	if err := findingsHTTPDB.Pool().QueryRow(context.Background(),
		`SELECT config::text FROM notification_channels WHERE tenant_id=$1 AND id=$2`,
		tenantID, created.ID).Scan(&storedConfig); err != nil {
		t.Fatalf("读取更新后配置失败: %v", err)
	}
	for _, plaintext := range []string{"plain-token-1", "plain-token-2"} {
		if strings.Contains(storedConfig, plaintext) {
			t.Fatalf("更新后 DB 残留明文 %q: %s", plaintext, storedConfig)
		}
	}

	// 旧明文行：读取时按明文处理并 best-effort 迁移为加密存储。
	legacy, err := store.CreateNotificationChannel(context.Background(), tenantID, pg.NotificationChannelInput{
		Name: "旧明文渠道", Type: "webhook",
		Config: map[string]any{"url": "https://legacy.example/hook", "password": "legacy-plain"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建旧明文渠道失败: %v", err)
	}
	rec = invokeHandler(t, opts.handleGetNotificationChannel, http.MethodGet,
		fmt.Sprintf("/api/v1/notifications/channels/%d", legacy.ID), "", map[string]string{"id": fmt.Sprint(legacy.ID)})
	requireStatus(t, rec, http.StatusOK)
	legacyView := decodeResponse[pg.NotificationChannel](t, rec)
	if legacyView.Config["password"] != "***" {
		t.Fatalf("旧明文响应未掩码: %+v", legacyView.Config)
	}
	var legacyStored string
	if err := findingsHTTPDB.Pool().QueryRow(context.Background(),
		`SELECT config::text FROM notification_channels WHERE tenant_id=$1 AND id=$2`,
		tenantID, legacy.ID).Scan(&legacyStored); err != nil {
		t.Fatalf("读取旧渠道存储失败: %v", err)
	}
	if strings.Contains(legacyStored, "legacy-plain") {
		t.Fatalf("旧明文读取后未迁移加密: %s", legacyStored)
	}

	// Evidence 未装配：含敏感配置拒绝（503）；非敏感配置允许。
	noEvidence := Options{Store: store, TenantID: tenantID}
	rec = invokeHandler(t, noEvidence.handleCreateNotificationChannel, http.MethodPost,
		"/api/v1/notifications/channels",
		`{"name":"无证据敏感","type":"webhook","config":{"url":"https://x.example","token":"t"}}`, nil)
	requireStatus(t, rec, http.StatusServiceUnavailable)
	requireErrorCode(t, rec, "evidence_unavailable")
	rec = invokeHandler(t, noEvidence.handleCreateNotificationChannel, http.MethodPost,
		"/api/v1/notifications/channels",
		`{"name":"无证据普通","type":"generic","config":{}}`, nil)
	requireStatus(t, rec, http.StatusCreated)

	// 轮换后通知配置必须用新密钥重加密：GET 仍能解密并掩码，DB 仍无明文。
	rotateBody := `{"new_key_hex":"` + hex.EncodeToString(b6Key(11)) + `"}`
	rec = invokeHandler(t, opts.handleEvidenceRotate, http.MethodPost, "/api/v1/evidence/rotate", rotateBody, nil)
	requireStatus(t, rec, http.StatusOK)
	rec = invokeHandler(t, opts.handleGetNotificationChannel, http.MethodGet,
		"/api/v1/notifications/channels/"+path["id"], "", path)
	requireStatus(t, rec, http.StatusOK)
	afterRotate := decodeResponse[pg.NotificationChannel](t, rec)
	if afterRotate.Config["token"] != "***" {
		t.Fatalf("轮换后渠道配置不可用: %+v", afterRotate.Config)
	}
	if err := findingsHTTPDB.Pool().QueryRow(context.Background(),
		`SELECT config::text FROM notification_channels WHERE tenant_id=$1 AND id=$2`,
		tenantID, created.ID).Scan(&storedConfig); err != nil {
		t.Fatalf("轮换后读取配置失败: %v", err)
	}
	if strings.Contains(storedConfig, "plain-token-2") {
		t.Fatalf("轮换后 DB 残留明文: %s", storedConfig)
	}
}

// TestB8NotificationDispatcher 覆盖事件推送、DB 去重、skipped 与 SSRF。
func TestB8NotificationDispatcher(t *testing.T) {
	t.Setenv("BEACONX_NOTIFY_ALLOW_PRIVATE", "true")
	store, tenantID, dir := b8Setup(t)
	opts := b8Options(t, store, tenantID, dir)
	ctx := context.Background()
	mission := mustMission(t, store, tenantID, "推送任务")

	received := make(chan map[string]any, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		received <- payload
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()

	rec := invokeHandler(t, opts.handleCreateNotificationChannel, http.MethodPost,
		"/api/v1/notifications/channels",
		`{"name":"推送 webhook","type":"webhook","config":{"url":"`+target.URL+`","token":"dispatch-secret"},"enabled":true}`, nil)
	requireStatus(t, rec, http.StatusCreated)
	webhook := decodeResponse[pg.NotificationChannel](t, rec)
	email, err := store.CreateNotificationChannel(ctx, tenantID, pg.NotificationChannelInput{
		Name: "推送 email", Type: "email", Config: map[string]any{"host": "localhost"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建 email 渠道失败: %v", err)
	}

	event, err := store.AppendMissionEvent(ctx, tenantID, mission.ID, task.EvCompleted, "system", "任务完成", nil)
	if err != nil {
		t.Fatalf("写入完成事件失败: %v", err)
	}
	// 非推送类型事件不产生投递。
	started, err := store.AppendMissionEvent(ctx, tenantID, mission.ID, task.EvStarted, "system", "开始", nil)
	if err != nil {
		t.Fatalf("写入开始事件失败: %v", err)
	}

	dispatcher := NewNotificationDispatcher(store, opts.Evidence, b7Logger())
	dispatcher.cursor = event.ID - 1
	dispatcher.processOnce(ctx)

	select {
	case payload := <-received:
		eventPayload, _ := payload["event"].(map[string]any)
		if eventPayload["type"] != string(task.EvCompleted) {
			t.Errorf("推送事件类型 = %v", eventPayload["type"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher 未推送完成事件")
	}
	deliveries, err := store.ListNotificationDeliveries(ctx, tenantID, 0, "", 100)
	if err != nil {
		t.Fatalf("读取投递记录失败: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("投递记录数 = %d，期望 2（webhook sent + email skipped）: %+v", len(deliveries), deliveries)
	}
	statusByChannel := map[int64]string{}
	for _, delivery := range deliveries {
		statusByChannel[delivery.ChannelID] = delivery.Status
	}
	if statusByChannel[webhook.ID] != "sent" || statusByChannel[email.ID] != "skipped" {
		t.Fatalf("投递状态不符合预期: %+v", statusByChannel)
	}

	// 重复轮询不重复投递（notification_events 唯一去重）。
	dispatcher.cursor = event.ID - 1
	dispatcher.processOnce(ctx)
	deliveries, _ = store.ListNotificationDeliveries(ctx, tenantID, 0, "", 100)
	if len(deliveries) != 2 {
		t.Fatalf("重复轮询产生了新投递: %+v", deliveries)
	}
	_ = started
	// 关闭已启用的渠道，隔离 SSRF 子场景（避免 webhook 再次收到推送）。
	disabled := false
	for _, channelID := range []int64{webhook.ID, email.ID} {
		if _, err := store.UpdateNotificationChannel(ctx, tenantID, channelID, pg.NotificationChannelUpdate{Enabled: &disabled}); err != nil {
			t.Fatalf("停用渠道失败: %v", err)
		}
	}

	// SSRF：私网目标记为 failed，不发起请求。
	t.Setenv("BEACONX_NOTIFY_ALLOW_PRIVATE", "false")
	blocked, err := store.CreateNotificationChannel(ctx, tenantID, pg.NotificationChannelInput{
		Name: "内网目标", Type: "webhook", Config: map[string]any{"url": "http://10.1.2.3/hook"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建内网渠道失败: %v", err)
	}
	second, err := store.AppendMissionEvent(ctx, tenantID, mission.ID, task.EvFailed, "system", "失败", nil)
	if err != nil {
		t.Fatalf("写入失败事件失败: %v", err)
	}
	dispatcher.cursor = second.ID - 1
	dispatcher.processOnce(ctx)
	deliveries, _ = store.ListNotificationDeliveries(ctx, tenantID, blocked.ID, "", 10)
	if len(deliveries) != 1 || deliveries[0].Status != "failed" || !strings.Contains(deliveries[0].Detail, "拒绝") {
		t.Fatalf("SSRF 投递记录不符合预期: %+v", deliveries)
	}
	select {
	case payload := <-received:
		t.Fatalf("被拒绝的目标仍收到请求: %+v", payload)
	case <-time.After(100 * time.Millisecond):
	}

	// Start/Stop 生命周期（只验证不 panic/不阻塞；事件处理逻辑已在上面的
	// processOnce 手工调用中覆盖）。Start 的游标语义是「从当前事件之后开始」，
	// 共享测试库里其它租户的新事件可能影响游标，因此这里不断言具体投递数。
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher.Start(lifecycleCtx)
	dispatcher.Stop()
	dispatcher.Stop() // 幂等
}

// TestB8EvidenceAuditAllActions 覆盖 create/rotate 审计与全租户重签。
func TestB8EvidenceAuditAllActions(t *testing.T) {
	store, ev, tenantA, _ := b6Setup(t)
	tenantB := mustExtraTenant(t)
	missionA := mustMission(t, store, tenantA, "审计任务 A")
	missionB := mustMission(t, store, tenantB, "审计任务 B")

	recordA := b5trafficCreateRecord(t, store, ev, tenantA, missionA.ID, "http_probe",
		`{"url":"https://example.com/a"}`, "HTTP 200\n\na", 200)
	recordB := b5trafficCreateRecord(t, store, ev, tenantB, missionB.ID, "http_probe",
		`{"url":"https://example.com/b"}`, "HTTP 200\n\nb", 200)
	refs := func(record pg.TrafficExchange) (evidence.BlobRef, evidence.BlobRef) {
		return evidence.BlobRef{Hash: record.RequestHash, Size: record.RequestSize},
			evidence.BlobRef{Hash: record.ResponseHash, Size: record.ResponseSize}
	}
	reqA, respA := refs(recordA)
	reqB, respB := refs(recordB)
	snapshotA := b5trafficSnapshot(t, store, ev, tenantA, missionA.ID, reqA, respA, "http_probe")
	snapshotB := b5trafficSnapshot(t, store, ev, tenantB, missionB.ID, reqB, respB, "http_probe")

	optsA := Options{Store: store, Evidence: ev, TenantID: tenantA}
	optsB := Options{Store: store, Evidence: ev, TenantID: tenantB}

	// create 审计：通过既有冻结接口创建第三条证据。
	createRec := invokeHandler(t, optsA.handleCreateEvidence, http.MethodPost,
		fmt.Sprintf("/api/v1/traffic/%d/evidence", recordA.ID), "", map[string]string{"id": fmt.Sprint(recordA.ID)})
	requireStatus(t, createRec, http.StatusCreated)
	created := decodeResponse[pg.EvidenceSnapshot](t, createRec)
	auditRec := invokeHandler(t, optsA.handleEvidenceAudit, http.MethodGet,
		fmt.Sprintf("/api/v1/evidence/%d/audit", created.ID), "", map[string]string{"id": fmt.Sprint(created.ID)})
	requireStatus(t, auditRec, http.StatusOK)
	createdAudit := decodeResponse[struct {
		Audit []pg.EvidenceAccessLog `json:"audit"`
	}](t, auditRec)
	if len(createdAudit.Audit) != 1 || createdAudit.Audit[0].Action != "create" {
		t.Fatalf("create 审计不符合预期: %+v", createdAudit.Audit)
	}

	// 轮换：两个租户的快照都必须重签，且各记 rotate 审计。
	body := `{"new_key_hex":"` + hex.EncodeToString(b6Key(9)) + `"}`
	rotateRec := invokeHandler(t, optsA.handleEvidenceRotate, http.MethodPost, "/api/v1/evidence/rotate", body, nil)
	requireStatus(t, rotateRec, http.StatusOK)
	var rotated struct {
		Rotated  int `json:"rotated"`
		Resigned int `json:"resigned"`
	}
	b5trafficDecodeInto(t, rotateRec, &rotated)
	if rotated.Resigned < 3 {
		t.Fatalf("resigned = %d，期望 ≥3（A 两条 + B 一条）", rotated.Resigned)
	}

	for _, item := range []struct {
		opts     Options
		snapshot pg.EvidenceSnapshot
	}{
		{optsA, snapshotA},
		{optsB, snapshotB},
	} {
		verifyRec := invokeHandler(t, item.opts.handleVerifyEvidence, http.MethodGet,
			fmt.Sprintf("/api/v1/evidence/%d/verify", item.snapshot.ID), "",
			map[string]string{"id": fmt.Sprint(item.snapshot.ID)})
		requireStatus(t, verifyRec, http.StatusOK)
		auditRec = invokeHandler(t, item.opts.handleEvidenceAudit, http.MethodGet,
			fmt.Sprintf("/api/v1/evidence/%d/audit", item.snapshot.ID), "",
			map[string]string{"id": fmt.Sprint(item.snapshot.ID)})
		requireStatus(t, auditRec, http.StatusOK)
		audit := decodeResponse[struct {
			Audit []pg.EvidenceAccessLog `json:"audit"`
		}](t, auditRec)
		hasRotate := false
		for _, entry := range audit.Audit {
			if entry.Action == "rotate" {
				hasRotate = true
			}
		}
		if !hasRotate {
			t.Errorf("快照 %d 缺少 rotate 审计: %+v", item.snapshot.ID, audit.Audit)
		}
	}
}

// TestB8StartupMaintenance 覆盖 LLM 正文保留期清理与 sync pending 恢复。
func TestB8StartupMaintenance(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	ctx := context.Background()
	mission := mustMission(t, store, tenantID, "维护任务")

	oldID, err := store.RecordLLMExchange(ctx, tenantID, pg.LLMExchangeInput{
		MissionID: mission.ID, Role: "worker", Model: "test", Request: []byte(`{"messages":[]}`),
		Response: "old", RequestBytes: 14, ResponseBytes: 3,
	})
	if err != nil {
		t.Fatalf("写入旧 LLM 正文失败: %v", err)
	}
	recentID, err := store.RecordLLMExchange(ctx, tenantID, pg.LLMExchangeInput{
		MissionID: mission.ID, Role: "worker", Model: "test", Request: []byte(`{"messages":[]}`),
		Response: "new", RequestBytes: 14, ResponseBytes: 3,
	})
	if err != nil {
		t.Fatalf("写入新 LLM 正文失败: %v", err)
	}
	if _, err := findingsHTTPDB.Pool().Exec(ctx,
		`UPDATE llm_exchanges SET created_at = now() - interval '10 days' WHERE tenant_id=$1 AND id=$2`,
		tenantID, oldID); err != nil {
		t.Fatalf("构造过期 LLM 正文失败: %v", err)
	}

	source, err := store.CreateSyncSource(ctx, tenantID, pg.SyncSourceInput{
		Name: "维护同步源", Type: pg.SyncSourceManual, Config: map[string]any{},
	})
	if err != nil {
		t.Fatalf("创建同步源失败: %v", err)
	}
	pendingRun, err := store.CreateSyncRun(ctx, tenantID, source.ID, nil)
	if err != nil {
		t.Fatalf("创建同步运行失败: %v", err)
	}
	if _, err := findingsHTTPDB.Pool().Exec(ctx,
		`UPDATE sync_runs SET status='pending', created_at=now() - interval '1 hour' WHERE tenant_id=$1 AND id=$2`,
		tenantID, pendingRun.ID); err != nil {
		t.Fatalf("构造超时 pending 失败: %v", err)
	}

	result, err := store.RunStartupMaintenance(ctx, tenantID, 7*24*time.Hour, 15*time.Minute)
	if err != nil {
		t.Fatalf("启动维护失败: %v", err)
	}
	if result.LLMDeleted != 1 || result.SyncPendingRecovered != 1 {
		t.Fatalf("维护结果 = %+v，期望 llm=1 pending=1", result)
	}
	// 旧正文删除、新正文保留。
	if _, err := store.GetLLMExchange(ctx, tenantID, oldID); !errors.Is(err, pg.ErrNotFound) {
		t.Errorf("过期 LLM 正文应被删除，实际: %v", err)
	}
	if _, err := store.GetLLMExchange(ctx, tenantID, recentID); err != nil {
		t.Errorf("未过期 LLM 正文不应被删除: %v", err)
	}
	restored, err := store.GetSyncRun(ctx, tenantID, pendingRun.ID)
	if err != nil {
		t.Fatalf("读取恢复后的同步运行失败: %v", err)
	}
	if restored.Status != pg.SyncRunFailed || restored.Error != "进程重启恢复" || restored.FinishedAt == nil {
		t.Fatalf("pending 恢复结果不符合预期: %+v", restored)
	}

	// retention=0 表示不清理。
	oldID2, err := store.RecordLLMExchange(ctx, tenantID, pg.LLMExchangeInput{
		MissionID: mission.ID, Role: "worker", Model: "test", Request: []byte(`{}`), Response: "x",
	})
	if err != nil {
		t.Fatalf("写入 LLM 正文失败: %v", err)
	}
	if _, err := findingsHTTPDB.Pool().Exec(ctx,
		`UPDATE llm_exchanges SET created_at = now() - interval '10 days' WHERE tenant_id=$1 AND id=$2`,
		tenantID, oldID2); err != nil {
		t.Fatalf("构造过期 LLM 正文失败: %v", err)
	}
	if result, err := store.RunStartupMaintenance(ctx, tenantID, 0, 0); err != nil || result.LLMDeleted != 0 {
		t.Fatalf("retention=0 应不清理: result=%+v err=%v", result, err)
	}
	if _, err := store.GetLLMExchange(ctx, tenantID, oldID2); err != nil {
		t.Errorf("retention=0 不应删除记录: %v", err)
	}
}
