package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B7 硬化测试：通知 SSRF/脱敏、Skills 元数据回滚、Workspace 跨进程配额。
//
// 复用两套既有 harness：
//   - findingsHTTPEnv / invokeHandler：直接调 handler，适合通知渠道；
//   - b5agentsNewHarness：真 HTTP + 临时状态目录，适合 workspace 与 skills。
// 未设置 BEACONX_TEST_PG_DSN 时整体跳过。

// ---------- 通知 SSRF ----------

// b7CreateWebhookChannel 直接经 store 创建 webhook 渠道（绕过创建接口的校验，
// 用于构造「非法但已存在」的历史配置）。
func b7CreateWebhookChannel(t *testing.T, store *pg.Store, tenantID int64, name, url string) pg.NotificationChannel {
	t.Helper()
	channel, err := store.CreateNotificationChannel(context.Background(), tenantID, pg.NotificationChannelInput{
		Name:    name,
		Type:    "webhook",
		Config:  map[string]any{"url": url},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建 webhook 渠道失败: %v", err)
	}
	return channel
}

// b7TestSend 触发一次测试发送，返回 recorder。
func b7TestSend(t *testing.T, opts Options, channelID int64) *httptest.ResponseRecorder {
	t.Helper()
	return invokeHandler(t, opts.handleTestNotificationChannel, http.MethodPost,
		fmt.Sprintf("/api/v1/notifications/channels/%d/test", channelID), "",
		map[string]string{"id": fmt.Sprint(channelID)})
}

// TestB7NotificationWebhookSSRF 验证私网/回环目标在发送前被拒绝（400）。
func TestB7NotificationWebhookSSRF(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := Options{Store: store, TenantID: tenantID}

	// 默认不允许私网：回环、私网、link-local、CGNAT、IPv6 回环都应 400。
	for _, target := range []string{
		"http://127.0.0.1:9/hook",
		"http://10.1.2.3/hook",
		"http://192.168.1.5/hook",
		"http://169.254.1.1/hook",
		"http://100.64.0.1/hook",
		"http://[::1]/hook",
	} {
		channel := b7CreateWebhookChannel(t, store, tenantID,
			fmt.Sprintf("ssrf-%d", time.Now().UnixNano()), target)
		rec := b7TestSend(t, opts, channel.ID)
		requireStatus(t, rec, http.StatusBadRequest)
		requireErrorCode(t, rec, "webhook_blocked")
	}
	// 被拒绝的发送不应写入投递记录。
	rec := invokeHandler(t, opts.handleListNotificationDeliveries, http.MethodGet,
		"/api/v1/notifications/deliveries", "", nil)
	requireStatus(t, rec, http.StatusOK)
	list := decodeResponse[struct {
		Deliveries []pg.NotificationDelivery `json:"deliveries"`
	}](t, rec)
	if len(list.Deliveries) != 0 {
		t.Fatalf("被拒绝的发送不应产生投递记录，实际 %+v", list.Deliveries)
	}

	// 显式放开私网后，回环 httptest 目标应真实发送成功。
	t.Setenv("BEACONX_NOTIFY_ALLOW_PRIVATE", "true")
	received := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	channel := b7CreateWebhookChannel(t, store, tenantID, "allow-private", target.URL)
	rec = b7TestSend(t, opts, channel.ID)
	requireStatus(t, rec, http.StatusOK)
	out := decodeResponse[struct {
		Sent     bool                    `json:"sent"`
		Delivery pg.NotificationDelivery `json:"delivery"`
	}](t, rec)
	if !out.Sent || out.Delivery.Status != "sent" {
		t.Fatalf("放开私网后发送结果 = sent:%v delivery:%+v", out.Sent, out.Delivery)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("放行后 webhook 未收到请求")
	}
}

// TestB7NotificationMasking 验证响应中的 config 敏感值被掩码，而存储保留原值。
func TestB7NotificationMasking(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := Options{Store: store, TenantID: tenantID}

	body := `{"name":"掩码渠道","type":"webhook","config":{
		"url":"http://user:pass@example.com/hook",
		"token":"s3cr3t-token",
		"nested":{"api_key":"nested-key","plain":"ok"},
		"plain":"visible"
	},"enabled":true}`
	rec := invokeHandler(t, opts.handleCreateNotificationChannel, http.MethodPost,
		"/api/v1/notifications/channels", body, nil)
	requireStatus(t, rec, http.StatusCreated)
	channel := decodeResponse[pg.NotificationChannel](t, rec)

	// URL 的 userinfo 被掩码，敏感键值为 ***，非敏感值保留。
	urlValue, _ := channel.Config["url"].(string)
	if strings.Contains(urlValue, "user") || strings.Contains(urlValue, "pass") {
		t.Fatalf("响应泄漏了 URL userinfo: %q", urlValue)
	}
	if !strings.Contains(urlValue, "***@example.com") {
		t.Fatalf("URL userinfo 未按 *** 掩码: %q", urlValue)
	}
	if channel.Config["token"] != "***" {
		t.Fatalf("token 未掩码: %v", channel.Config["token"])
	}
	if channel.Config["plain"] != "visible" {
		t.Fatalf("非敏感键被误掩码: %v", channel.Config["plain"])
	}
	nested, _ := channel.Config["nested"].(map[string]any)
	if nested["api_key"] != "***" || nested["plain"] != "ok" {
		t.Fatalf("嵌套对象掩码不正确: %+v", nested)
	}

	// 详情与列表同样掩码。
	pathValues := map[string]string{"id": fmt.Sprint(channel.ID)}
	rec = invokeHandler(t, opts.handleGetNotificationChannel, http.MethodGet,
		"/api/v1/notifications/channels/"+fmt.Sprint(channel.ID), "", pathValues)
	requireStatus(t, rec, http.StatusOK)
	got := decodeResponse[pg.NotificationChannel](t, rec)
	if got.Config["token"] != "***" {
		t.Fatalf("详情 token 未掩码: %v", got.Config["token"])
	}
	rec = invokeHandler(t, opts.handleListNotificationChannels, http.MethodGet,
		"/api/v1/notifications/channels", "", nil)
	requireStatus(t, rec, http.StatusOK)
	listed := decodeResponse[struct {
		Channels []pg.NotificationChannel `json:"channels"`
	}](t, rec)
	if len(listed.Channels) != 1 || listed.Channels[0].Config["token"] != "***" {
		t.Fatalf("列表掩码不正确: %+v", listed.Channels)
	}

	// 存储保持原值（掩码只发生在响应投影）。
	stored, err := store.GetNotificationChannel(context.Background(), tenantID, channel.ID)
	if err != nil {
		t.Fatalf("读取存储渠道失败: %v", err)
	}
	if stored.Config["token"] != "s3cr3t-token" {
		t.Fatalf("存储被掩码污染: %v", stored.Config["token"])
	}
	if urlText, _ := stored.Config["url"].(string); !strings.Contains(urlText, "user:pass@") {
		t.Fatalf("存储的 URL 被改写: %q", urlText)
	}
}

// ---------- Skills 元数据回滚 ----------

// TestB7SkillsRestoreMeta 验证 restore 同时回滚文件内容与 DB 元数据。
func TestB7SkillsRestoreMeta(t *testing.T) {
	h := b5agentsNewHarness(t)
	t.Setenv("BEACONX_SKILLS_DIR", t.TempDir())

	b6Do(t, h, http.MethodPost, "/api/v1/skills",
		`{"name":"meta-skill","description":"desc-1"}`, http.StatusCreated)
	b6SkillsWriteFile(t, h, "meta-skill", "a.md", "one", http.StatusOK)
	// 第二次写入产生的快照：内容是 a.md=one，元数据仍是 desc-1。
	b6SkillsWriteFile(t, h, "meta-skill", "a.md", "two", http.StatusOK)
	// 之后才改元数据：这样「恢复该快照」能同时观察到文件与元数据两处回滚。
	b6Do(t, h, http.MethodPatch, "/api/v1/skills/meta-skill",
		`{"description":"desc-2","version":"2.0.0"}`, http.StatusOK)

	versions := b6SkillsListVersions(t, h, "meta-skill")
	if len(versions) != 2 {
		t.Fatalf("版本数 = %d，期望 2（%v）", len(versions), versions)
	}
	// 时间戳降序：最新一份是「第二次 PUT 之前」的状态 = desc-1 + a.md=one。
	beforeSecondWrite := versions[0]["timestamp"].(string)

	rec := b6Do(t, h, http.MethodPost,
		"/api/v1/skills/meta-skill/versions/"+beforeSecondWrite+"/restore", "", http.StatusOK)
	var restored struct {
		Description string `json:"description"`
		Version     string `json:"version"`
		Warning     string `json:"warning"`
	}
	if err := json.Unmarshal(rec, &restored); err != nil {
		t.Fatalf("解析恢复响应失败: %v", err)
	}
	if restored.Description != "desc-1" || restored.Version != "0.1.0" {
		t.Fatalf("元数据未回滚: %+v", restored)
	}
	if restored.Warning != "" {
		t.Fatalf("有 meta.json 的版本不应有 warning: %q", restored.Warning)
	}
	if got := b6SkillsReadFile(t, h, "meta-skill", "a.md"); got != "one" {
		t.Fatalf("文件内容未回滚: %q", got)
	}
	// meta.json 不能被写回技能目录（文件列表里不应出现）。
	raw := b6Do(t, h, http.MethodGet, "/api/v1/skills/meta-skill/files", "", http.StatusOK)
	if strings.Contains(string(raw), "meta.json") {
		t.Fatalf("meta.json 泄漏到技能文件列表: %s", raw)
	}
}

// TestB7SkillsRestoreLegacyWarning 验证旧版本（无 meta.json）只回滚文件并返回 warning。
func TestB7SkillsRestoreLegacyWarning(t *testing.T) {
	h := b5agentsNewHarness(t)
	skillsDir := t.TempDir()
	t.Setenv("BEACONX_SKILLS_DIR", skillsDir)

	b6Do(t, h, http.MethodPost, "/api/v1/skills",
		`{"name":"legacy-skill","description":"old-desc"}`, http.StatusCreated)
	b6SkillsWriteFile(t, h, "legacy-skill", "a.md", "old-content", http.StatusOK)
	// 再写一次：快照里才包含 a.md=old-content，用于验证「旧版本只回滚文件」。
	b6SkillsWriteFile(t, h, "legacy-skill", "a.md", "newer-content", http.StatusOK)
	versions := b6SkillsListVersions(t, h, "legacy-skill")
	if len(versions) != 2 {
		t.Fatalf("版本数 = %d，期望 2（%v）", len(versions), versions)
	}
	stamp := versions[0]["timestamp"].(string)

	// 模拟 B7 之前打的快照：手工删除 meta.json。
	metaPath := filepath.Join(skillsDir, fmt.Sprintf("%d", h.tenantA), "legacy-skill", ".versions", stamp, "meta.json")
	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("删除 meta.json 失败: %v", err)
	}
	// 改元数据与文件，随后恢复旧版本。
	b6Do(t, h, http.MethodPatch, "/api/v1/skills/legacy-skill",
		`{"description":"new-desc"}`, http.StatusOK)
	b6SkillsWriteFile(t, h, "legacy-skill", "a.md", "new-content", http.StatusOK)

	rec := b6Do(t, h, http.MethodPost,
		"/api/v1/skills/legacy-skill/versions/"+stamp+"/restore", "", http.StatusOK)
	var restored struct {
		Description string `json:"description"`
		Warning     string `json:"warning"`
	}
	if err := json.Unmarshal(rec, &restored); err != nil {
		t.Fatalf("解析恢复响应失败: %v", err)
	}
	if !strings.Contains(restored.Warning, "meta.json") {
		t.Fatalf("缺少 meta.json 的版本应返回 warning，实际 %q", restored.Warning)
	}
	if got := b6SkillsReadFile(t, h, "legacy-skill", "a.md"); got != "old-content" {
		t.Fatalf("文件内容未回滚: %q", got)
	}
	detail := b6Do(t, h, http.MethodGet, "/api/v1/skills/legacy-skill", "", http.StatusOK)
	var skill struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(detail, &skill); err != nil {
		t.Fatalf("解析技能详情失败: %v", err)
	}
	if skill.Description != "new-desc" {
		t.Fatalf("无 meta 版本不应回滚元数据，实际 description=%q", skill.Description)
	}
}

// ---------- Workspace 跨进程配额 ----------

// TestB7WorkspaceAdvisoryLock 验证 PG advisory lock 能把临界区串行化。
func TestB7WorkspaceAdvisoryLock(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	ctx := context.Background()

	var (
		mu        sync.Mutex
		active    int
		maxActive int
		errs      []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := store.WithWorkspaceLock(ctx, tenantID, 987654321, func(context.Context) error {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()
				time.Sleep(80 * time.Millisecond)
				mu.Lock()
				active--
				mu.Unlock()
				return nil
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("获取工作区锁失败: %v", errs)
	}
	if maxActive != 1 {
		t.Fatalf("advisory lock 未串行化：最大并发 = %d，期望 1", maxActive)
	}

	// 参数校验与错误传播。
	if err := store.WithWorkspaceLock(ctx, 0, 1, nil); err == nil {
		t.Fatal("非法租户应返回错误")
	}
	sentinel := fmt.Errorf("业务错误")
	if err := store.WithWorkspaceLock(ctx, tenantID, 1, func(context.Context) error { return sentinel }); !strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("fn 错误未向外传播: %v", err)
	}
	// 出错后锁必须释放：再次获取不应阻塞。
	done := make(chan struct{})
	go func() {
		_ = store.WithWorkspaceLock(ctx, tenantID, 1, func(context.Context) error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("上一次失败后 advisory lock 未释放")
	}
}

// TestB7WorkspaceQuotaTwoWriters 验证两个并发写入在 1MB 配额下只成功一个。
func TestB7WorkspaceQuotaTwoWriters(t *testing.T) {
	h := b5agentsNewHarness(t)
	t.Setenv("BEACONX_WORKSPACE_QUOTA_MB", "1")
	mission := h.createMission(t, h.tenantA, "跨进程配额任务")

	const size = 700 << 10 // 700KB × 2 > 1MB
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses []int
	)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			url := h.baseURL + b6WorkspacePath(mission.ID, fmt.Sprintf("two/%d.txt", index))
			req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(strings.Repeat("Q", size)))
			if err != nil {
				return
			}
			req.Header.Set("Authorization", "Bearer "+testAPIToken)
			<-start
			resp, err := h.client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			mu.Lock()
			statuses = append(statuses, resp.StatusCode)
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	if len(statuses) != 2 {
		t.Fatalf("并发请求未全部完成: %v", statuses)
	}
	success := 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			success++
		case http.StatusRequestEntityTooLarge:
		default:
			t.Fatalf("出现意外状态码 %d（全部=%v）", status, statuses)
		}
	}
	if success != 1 {
		t.Fatalf("1MB 配额下两个 700KB 并发写成功数 = %d，期望 1（全部=%v）", success, statuses)
	}
	quota, used := b6WorkspaceQuota(t, h, mission.ID)
	if used > quota {
		t.Fatalf("超卖：used=%d > quota=%d", used, quota)
	}
}
