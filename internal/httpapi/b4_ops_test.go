package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/config"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/internal/logging"
)

// B4 日志 / 通知接口测试。
//
// 日志接口走真实的 NewHandler（验证 feature 路由注册 + 鉴权 + SSE），
// 通知接口既覆盖直接调用 Options 处理器（Store 层语义），也覆盖一次真实
// HTTP 路由注册；通知的 PG 集成测试复用 findings_api_test.go 的租户/迁移环境。

const b4Token = "b4-test-token"

// b4Logger 创建会写入环形缓冲的日志器（logging.New 自带 tee）。
func b4Logger(t *testing.T) *slog.Logger {
	t.Helper()
	logger, err := logging.New(config.Log{Level: "debug", Format: "json"})
	if err != nil {
		t.Fatalf("创建日志器失败: %v", err)
	}
	return logger
}

// b4Server 构建带鉴权与 feature 路由的完整 HTTP 处理器。
func b4Server(t *testing.T, store *pg.Store, tenantID int64) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	handler, err := NewHandler(Options{
		Logger:         logger,
		RequestTimeout: 10 * time.Second,
		Authenticator:  NewTokenAuthenticator(b4Token),
		Store:          store,
		TenantID:       tenantID,
	})
	if err != nil {
		t.Fatalf("构建 HTTP 处理器失败: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// b4Request 向真实服务发一次请求；token 为空表示不带鉴权。
func b4Request(t *testing.T, srv *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp
}

// b4Decode 解码 JSON 响应并关闭响应体；失败时带上原始正文。
func b4HTTPDecode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应失败: %v，body=%s", err, raw)
	}
	return out
}

// TestLogsAPI 覆盖日志查询：401、级别过滤、limit 上限、属性简化与非法参数。
func TestLogsAPI(t *testing.T) {
	srv := b4Server(t, nil, 0)

	// 无 token：401（feature 路由复用了统一鉴权）。
	resp := b4Request(t, srv, http.MethodGet, "/api/v1/logs", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未授权状态码 = %d，期望 401", resp.StatusCode)
	}

	logger := b4Logger(t)
	logger.Debug("b4-log-debug-marker")
	logger.Info("b4-log-info-marker")
	logger.Warn("b4-log-warn-marker")
	logger.Error("b4-log-error-marker")
	logger.Info("b4-log-attrs-marker", "component", "b4", "duration", time.Second, "ok", true)

	type logsResponse struct {
		Logs []logging.Entry `json:"logs"`
	}

	// 级别过滤：只返回 warn。
	resp = b4Request(t, srv, http.MethodGet, "/api/v1/logs?level=warn&limit=100", b4Token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查询日志状态码 = %d", resp.StatusCode)
	}
	warnLogs := b4HTTPDecode[logsResponse](t, resp)
	if len(warnLogs.Logs) == 0 {
		t.Fatal("warn 过滤结果为空")
	}
	sawWarn := false
	for _, entry := range warnLogs.Logs {
		if entry.Level != "warn" {
			t.Fatalf("过滤后出现非 warn 条目: %+v", entry)
		}
		if entry.Msg == "b4-log-warn-marker" {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Fatal("未找到 warn 标记日志")
	}

	// 不过滤：四条标记都应存在；属性按简化规则落成 map。
	resp = b4Request(t, srv, http.MethodGet, "/api/v1/logs?limit=1000", b4Token, "")
	allLogs := b4HTTPDecode[logsResponse](t, resp)
	seen := map[string]bool{}
	for _, entry := range allLogs.Logs {
		seen[entry.Msg] = true
		if entry.Msg == "b4-log-attrs-marker" {
			if entry.Attrs["component"] != "b4" {
				t.Errorf("attrs.component = %#v，期望 b4", entry.Attrs["component"])
			}
			if entry.Attrs["duration"] != "1s" {
				t.Errorf("attrs.duration = %#v，期望 1s", entry.Attrs["duration"])
			}
			if entry.Attrs["ok"] != true {
				t.Errorf("attrs.ok = %#v，期望 true", entry.Attrs["ok"])
			}
		}
	}
	for _, marker := range []string{"b4-log-debug-marker", "b4-log-info-marker", "b4-log-warn-marker", "b4-log-error-marker"} {
		if !seen[marker] {
			t.Errorf("缺少日志标记 %s", marker)
		}
	}

	// 非法参数：400；limit 超上限被钳制。
	resp = b4Request(t, srv, http.MethodGet, "/api/v1/logs?level=bogus", b4Token, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 level 状态码 = %d，期望 400", resp.StatusCode)
	}
	resp = b4Request(t, srv, http.MethodGet, "/api/v1/logs?limit=abc", b4Token, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 limit 状态码 = %d，期望 400", resp.StatusCode)
	}
	resp = b4Request(t, srv, http.MethodGet, "/api/v1/logs?limit=99999", b4Token, "")
	clamped := b4HTTPDecode[logsResponse](t, resp)
	if len(clamped.Logs) > logsMaxLimit {
		t.Errorf("limit 未钳制：返回 %d 条，上限 %d", len(clamped.Logs), logsMaxLimit)
	}
}

// TestLogsStreamSSE 覆盖日志 SSE：401、心跳帧、event: log 与实时条目。
func TestLogsStreamSSE(t *testing.T) {
	previous := logsSSEHeartbeat
	logsSSEHeartbeat = 25 * time.Millisecond
	t.Cleanup(func() { logsSSEHeartbeat = previous })

	srv := b4Server(t, nil, 0)

	resp := b4Request(t, srv, http.MethodGet, "/api/v1/logs/stream", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未授权 SSE 状态码 = %d，期望 401", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/logs/stream?token="+b4Token, nil)
	if err != nil {
		t.Fatalf("构造 SSE 请求失败: %v", err)
	}
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatalf("建立 SSE 连接失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}

	lines := make(chan string, 128)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimRight(line, "\r\n")
		}
	}()

	waitLine := func(match func(string) bool, what string) string {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("SSE 流提前结束（等待 %s）", what)
				}
				if match(line) {
					return line
				}
			case <-deadline:
				t.Fatalf("等待 %s 超时", what)
			}
		}
	}

	// 先看到心跳，确保处理器已经进入 select 循环并完成订阅。
	waitLine(func(line string) bool { return line == ": ping" }, "心跳帧")

	// 再写一条日志，应收到 event: log 与对应 data。
	logger := b4Logger(t)
	logger.Info("b4-sse-marker", "k", "v")
	waitLine(func(line string) bool { return line == "event: log" }, "event: log 帧")
	dataLine := waitLine(func(line string) bool {
		return strings.HasPrefix(line, "data: ") && strings.Contains(line, "b4-sse-marker")
	}, "日志 data 帧")
	if !strings.Contains(dataLine, `"level":"info"`) {
		t.Errorf("日志帧缺少级别字段: %s", dataLine)
	}

	// 主动断开：取消请求上下文即可让 handler 退出。
	cancel()
}

// TestNotificationChannelsCRUD 覆盖渠道 CRUD、名称冲突、非法参数、跨租户隔离。
func TestNotificationChannelsCRUD(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	opts := Options{Store: store, TenantID: tenantID}

	createBody := `{"name":"运维 webhook","type":"webhook","config":{"url":"http://127.0.0.1:9/hook"},"enabled":true}`
	rec := invokeHandler(t, opts.handleCreateNotificationChannel, http.MethodPost,
		"/api/v1/notifications/channels", createBody, nil)
	requireStatus(t, rec, http.StatusCreated)
	channel := decodeResponse[pg.NotificationChannel](t, rec)
	if channel.ID == 0 || channel.Name != "运维 webhook" || channel.Type != "webhook" || !channel.Enabled {
		t.Fatalf("创建渠道结果不符合预期: %+v", channel)
	}
	channelPath := map[string]string{"id": fmt.Sprint(channel.ID)}

	// 名称重复：409。
	rec = invokeHandler(t, opts.handleCreateNotificationChannel, http.MethodPost,
		"/api/v1/notifications/channels", createBody, nil)
	requireStatus(t, rec, http.StatusConflict)
	requireErrorCode(t, rec, "conflict")

	// 非法参数：400。
	for _, body := range []string{
		`{`,
		`{"name":"","type":"webhook","config":{"url":"http://x"}}`,
		`{"name":"im 渠道","type":"im","config":{}}`,
		`{"name":"缺 url","type":"webhook","config":{}}`,
		`{"name":"坏 url","type":"webhook","config":{"url":"ftp://example.com/x"}}`,
	} {
		rec = invokeHandler(t, opts.handleCreateNotificationChannel, http.MethodPost,
			"/api/v1/notifications/channels", body, nil)
		requireStatus(t, rec, http.StatusBadRequest)
	}

	// 列表 / 详情。
	rec = invokeHandler(t, opts.handleListNotificationChannels, http.MethodGet,
		"/api/v1/notifications/channels?limit=10", "", nil)
	requireStatus(t, rec, http.StatusOK)
	list := decodeResponse[struct {
		Channels []pg.NotificationChannel `json:"channels"`
	}](t, rec)
	if len(list.Channels) != 1 || list.Channels[0].ID != channel.ID {
		t.Fatalf("渠道列表不符合预期: %+v", list.Channels)
	}
	rec = invokeHandler(t, opts.handleGetNotificationChannel, http.MethodGet,
		"/api/v1/notifications/channels/"+channelPath["id"], "", channelPath)
	requireStatus(t, rec, http.StatusOK)

	// patch：改名 + 关闭；webhook 的 url 是必填配置，不允许清空。
	rec = invokeHandler(t, opts.handleUpdateNotificationChannel, http.MethodPatch,
		"/api/v1/notifications/channels/"+channelPath["id"],
		`{"name":"运维 webhook 2","enabled":false}`, channelPath)
	requireStatus(t, rec, http.StatusOK)
	updated := decodeResponse[pg.NotificationChannel](t, rec)
	if updated.Name != "运维 webhook 2" || updated.Enabled {
		t.Fatalf("更新结果不符合预期: %+v", updated)
	}
	// 清空 webhook 配置会丢失必填 url：null 与 {} 都应 400。
	rec = invokeHandler(t, opts.handleUpdateNotificationChannel, http.MethodPatch,
		"/api/v1/notifications/channels/"+channelPath["id"], `{"config":null}`, channelPath)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleUpdateNotificationChannel, http.MethodPatch,
		"/api/v1/notifications/channels/"+channelPath["id"], `{"config":{}}`, channelPath)
	requireStatus(t, rec, http.StatusBadRequest)
	// 写回合法配置并重新启用。
	rec = invokeHandler(t, opts.handleUpdateNotificationChannel, http.MethodPatch,
		"/api/v1/notifications/channels/"+channelPath["id"],
		`{"config":{"url":"https://example.com/hook"},"enabled":true}`, channelPath)
	requireStatus(t, rec, http.StatusOK)
	// 数组配置：400。
	rec = invokeHandler(t, opts.handleUpdateNotificationChannel, http.MethodPatch,
		"/api/v1/notifications/channels/"+channelPath["id"], `{"config":[]}`, channelPath)
	requireStatus(t, rec, http.StatusBadRequest)

	// email 渠道的 config 不是必填：null 可以清空为 {}。
	emailChannel, err := store.CreateNotificationChannel(context.Background(), tenantID, pg.NotificationChannelInput{
		Name: "邮件渠道", Type: "email", Config: map[string]any{"smtp": map[string]any{"host": "localhost"}}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建 email 渠道失败: %v", err)
	}
	rec = invokeHandler(t, opts.handleUpdateNotificationChannel, http.MethodPatch,
		"/api/v1/notifications/channels/"+fmt.Sprint(emailChannel.ID), `{"config":null}`,
		map[string]string{"id": fmt.Sprint(emailChannel.ID)})
	requireStatus(t, rec, http.StatusOK)
	cleared := decodeResponse[pg.NotificationChannel](t, rec)
	if len(cleared.Config) != 0 {
		t.Errorf("email 渠道清空后 config = %+v，期望空对象", cleared.Config)
	}

	// 不存在的渠道：404；非法路径 id：400；Store 未装配：503。
	rec = invokeHandler(t, opts.handleGetNotificationChannel, http.MethodGet,
		"/api/v1/notifications/channels/999999999", "", map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
	rec = invokeHandler(t, opts.handleGetNotificationChannel, http.MethodGet,
		"/api/v1/notifications/channels/not-a-number", "", map[string]string{"id": "not-a-number"})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, Options{}.handleListNotificationChannels, http.MethodGet,
		"/api/v1/notifications/channels", "", nil)
	requireStatus(t, rec, http.StatusServiceUnavailable)

	// 跨租户隔离：另一个租户看不到该渠道。
	otherTenant := mustExtraTenant(t)
	otherOpts := Options{Store: store, TenantID: otherTenant}
	rec = invokeHandler(t, otherOpts.handleGetNotificationChannel, http.MethodGet,
		"/api/v1/notifications/channels/"+channelPath["id"], "", channelPath)
	requireStatus(t, rec, http.StatusNotFound)
	rec = invokeHandler(t, otherOpts.handleListNotificationChannels, http.MethodGet,
		"/api/v1/notifications/channels", "", nil)
	requireStatus(t, rec, http.StatusOK)
	empty := decodeResponse[struct {
		Channels []pg.NotificationChannel `json:"channels"`
	}](t, rec)
	if len(empty.Channels) != 0 {
		t.Fatalf("跨租户列表应为空，实际 %+v", empty.Channels)
	}

	// 真实路由注册 + 鉴权（feature 路由由 NewHandler 绑定）。
	srv := b4Server(t, store, tenantID)
	resp := b4Request(t, srv, http.MethodGet, "/api/v1/notifications/channels", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未授权渠道列表状态码 = %d，期望 401", resp.StatusCode)
	}
	resp = b4Request(t, srv, http.MethodGet, "/api/v1/notifications/channels", b4Token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("渠道列表状态码 = %d", resp.StatusCode)
	}
	_ = b4HTTPDecode[struct {
		Channels []pg.NotificationChannel `json:"channels"`
	}](t, resp)

	// 删除：204；再查 / 再删 404。
	rec = invokeHandler(t, opts.handleDeleteNotificationChannel, http.MethodDelete,
		"/api/v1/notifications/channels/"+channelPath["id"], "", channelPath)
	requireStatus(t, rec, http.StatusNoContent)
	rec = invokeHandler(t, opts.handleGetNotificationChannel, http.MethodGet,
		"/api/v1/notifications/channels/"+channelPath["id"], "", channelPath)
	requireStatus(t, rec, http.StatusNotFound)
	rec = invokeHandler(t, opts.handleDeleteNotificationChannel, http.MethodDelete,
		"/api/v1/notifications/channels/"+channelPath["id"], "", channelPath)
	requireStatus(t, rec, http.StatusNotFound)
}

// TestNotificationChannelTestDelivery 覆盖测试发送的 sent/failed/skipped 语义
// 与投递记录过滤、级联删除。
func TestNotificationChannelTestDelivery(t *testing.T) {
	// B7 硬化后 webhook 默认拒绝回环/私网目标；本测试用 httptest（127.0.0.1）
	// 验证真实投递链路，因此显式放开私网访问。
	t.Setenv("BEACONX_NOTIFY_ALLOW_PRIVATE", "true")
	store, tenantID := findingsHTTPEnv(t)
	opts := Options{Store: store, TenantID: tenantID}

	mustChannel := func(name, channelType string, config map[string]any) pg.NotificationChannel {
		t.Helper()
		created, err := store.CreateNotificationChannel(context.Background(), tenantID, pg.NotificationChannelInput{
			Name: name, Type: channelType, Config: config, Enabled: true,
		})
		if err != nil {
			t.Fatalf("创建渠道失败: %v", err)
		}
		return created
	}
	testChannel := func(id int64) (bool, pg.NotificationDelivery) {
		t.Helper()
		rec := invokeHandler(t, opts.handleTestNotificationChannel, http.MethodPost,
			fmt.Sprintf("/api/v1/notifications/channels/%d/test", id), "",
			map[string]string{"id": fmt.Sprint(id)})
		requireStatus(t, rec, http.StatusOK)
		out := decodeResponse[struct {
			Sent      bool                    `json:"sent"`
			ChannelID int64                   `json:"channel_id"`
			Delivery  pg.NotificationDelivery `json:"delivery"`
		}](t, rec)
		if out.ChannelID != id {
			t.Fatalf("channel_id = %d，期望 %d", out.ChannelID, id)
		}
		return out.Sent, out.Delivery
	}

	// 成功 webhook：真实 POST，断言收到的 payload。
	received := make(chan map[string]any, 1)
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("webhook 方法 = %s，期望 POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("webhook Content-Type = %q", ct)
		}
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		received <- payload
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer okServer.Close()
	okChannel := mustChannel("成功 webhook", "webhook", map[string]any{"url": okServer.URL})
	sent, delivery := testChannel(okChannel.ID)
	if !sent || delivery.Status != "sent" || delivery.Event != "test" {
		t.Fatalf("成功 webhook 结果 = sent:%v delivery:%+v", sent, delivery)
	}
	select {
	case payload := <-received:
		if payload["event"] != "test" {
			t.Errorf("webhook payload.event = %v，期望 test", payload["event"])
		}
		channelPayload, _ := payload["channel"].(map[string]any)
		if channelPayload["id"] != float64(okChannel.ID) || channelPayload["name"] != "成功 webhook" {
			t.Errorf("webhook payload.channel = %v", payload["channel"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook 未收到测试 POST")
	}

	// 失败 webhook：返回 500，记 failed，响应 sent=false。
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer failServer.Close()
	failChannel := mustChannel("失败 webhook", "webhook", map[string]any{"url": failServer.URL})
	sent, failedDelivery := testChannel(failChannel.ID)
	if sent || failedDelivery.Status != "failed" || !strings.Contains(failedDelivery.Detail, "HTTP 500") {
		t.Fatalf("失败 webhook 结果 = sent:%v delivery:%+v", sent, failedDelivery)
	}

	// email / generic：不联网，记 skipped。
	emailChannel := mustChannel("邮件渠道", "email", map[string]any{"smtp": map[string]any{"host": "localhost", "port": 25}})
	if sent, delivery := testChannel(emailChannel.ID); sent || delivery.Status != "skipped" {
		t.Fatalf("email 渠道结果 = sent:%v delivery:%+v", sent, delivery)
	}
	genericChannel := mustChannel("通用渠道", "generic", map[string]any{})
	if sent, delivery := testChannel(genericChannel.ID); sent || delivery.Status != "skipped" {
		t.Fatalf("generic 渠道结果 = sent:%v delivery:%+v", sent, delivery)
	}

	// 投递记录过滤。
	listDeliveries := func(query string) []pg.NotificationDelivery {
		t.Helper()
		rec := invokeHandler(t, opts.handleListNotificationDeliveries, http.MethodGet,
			"/api/v1/notifications/deliveries"+query, "", nil)
		requireStatus(t, rec, http.StatusOK)
		return decodeResponse[struct {
			Deliveries []pg.NotificationDelivery `json:"deliveries"`
		}](t, rec).Deliveries
	}
	if got := listDeliveries("?channel_id=" + fmt.Sprint(okChannel.ID)); len(got) != 1 || got[0].Status != "sent" {
		t.Fatalf("按渠道过滤结果 = %+v", got)
	}
	if got := listDeliveries("?status=failed"); len(got) != 1 || got[0].ChannelID != failChannel.ID {
		t.Fatalf("按状态过滤结果 = %+v", got)
	}
	if got := listDeliveries(""); len(got) != 4 {
		t.Fatalf("投递记录总数 = %d，期望 4", len(got))
	}
	// 非法过滤参数：400。
	rec := invokeHandler(t, opts.handleListNotificationDeliveries, http.MethodGet,
		"/api/v1/notifications/deliveries?status=bogus", "", nil)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = invokeHandler(t, opts.handleListNotificationDeliveries, http.MethodGet,
		"/api/v1/notifications/deliveries?channel_id=abc", "", nil)
	requireStatus(t, rec, http.StatusBadRequest)

	// 跨租户：另一个租户看不到这些投递记录。
	otherTenant := mustExtraTenant(t)
	otherOpts := Options{Store: store, TenantID: otherTenant}
	rec = invokeHandler(t, otherOpts.handleListNotificationDeliveries, http.MethodGet,
		"/api/v1/notifications/deliveries", "", nil)
	requireStatus(t, rec, http.StatusOK)
	crossTenant := decodeResponse[struct {
		Deliveries []pg.NotificationDelivery `json:"deliveries"`
	}](t, rec)
	if len(crossTenant.Deliveries) != 0 {
		t.Fatalf("跨租户投递记录应为空，实际 %+v", crossTenant.Deliveries)
	}

	// 空投递记录返回 [] 而不是 null。
	emptyChannel := mustChannel("空记录渠道", "generic", map[string]any{})
	rec = invokeHandler(t, opts.handleListNotificationDeliveries, http.MethodGet,
		"/api/v1/notifications/deliveries?channel_id="+fmt.Sprint(emptyChannel.ID), "", nil)
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"deliveries":[]`) {
		t.Errorf("空投递列表应输出 []，实际: %s", rec.Body.String())
	}

	// 删除渠道级联删除投递记录。
	if err := store.DeleteNotificationChannel(context.Background(), tenantID, okChannel.ID); err != nil {
		t.Fatalf("删除渠道失败: %v", err)
	}
	if got := listDeliveries("?channel_id=" + fmt.Sprint(okChannel.ID)); len(got) != 0 {
		t.Fatalf("删除渠道后投递记录应被级联删除，实际 %+v", got)
	}

	// 不存在的渠道测试：404。
	rec = invokeHandler(t, opts.handleTestNotificationChannel, http.MethodPost,
		"/api/v1/notifications/channels/999999999/test", "",
		map[string]string{"id": "999999999"})
	requireStatus(t, rec, http.StatusNotFound)
}
