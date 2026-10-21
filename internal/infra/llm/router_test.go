package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newJSONLogger 返回写入内存缓冲的 JSON 结构化日志器，便于断言日志字段。
func newJSONLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// parseRouterLogs 把缓冲里的 JSON 日志逐行解析为记录。
func parseRouterLogs(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	text := strings.TrimSpace(buf.String())
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("解析日志失败：%v（%q）", err, line)
		}
		out = append(out, record)
	}
	return out
}

// newTestRouter 用 5s 超时构造测试 Router。
func newTestRouter(t *testing.T, profiles []Profile, maxRetries int, log *slog.Logger) *Router {
	t.Helper()
	router, err := NewRouter(profiles, 5*time.Second, maxRetries, log)
	if err != nil {
		t.Fatalf("NewRouter：%v", err)
	}
	return router
}

// countingStatusServer 返回固定状态码并统计请求次数。
func countingStatusServer(t *testing.T, calls *int32, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// countingSuccessServer 返回固定成功响应并统计请求次数。
func countingSuccessServer(t *testing.T, calls *int32, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		writeChatSuccess(w, "m", content, "stop")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRouterSwitchesOnTransientThenSucceeds(t *testing.T) {
	var firstCalls, secondCalls int32
	first := countingStatusServer(t, &firstCalls, http.StatusInternalServerError, "临时故障")
	second := countingSuccessServer(t, &secondCalls, "第二个 profile 的答案")
	logger, buf := newJSONLogger()

	router := newTestRouter(t, []Profile{
		{Name: "primary", BaseURL: first.URL, Model: "m"},
		{Name: "backup", BaseURL: second.URL, Model: "m"},
	}, 1, logger)

	resp, err := router.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete：%v", err)
	}
	if resp.Content != "第二个 profile 的答案" {
		t.Errorf("Content = %q", resp.Content)
	}
	if atomic.LoadInt32(&secondCalls) != 1 {
		t.Errorf("第二个 profile 请求次数 = %d，期望 1", secondCalls)
	}
	if atomic.LoadInt32(&firstCalls) == 0 {
		t.Error("第一个 profile 未被尝试")
	}

	logs := parseRouterLogs(t, buf)
	if len(logs) != 1 {
		t.Fatalf("切换日志条数 = %d，期望 1（禁止按内部重试刷日志）：%v", len(logs), logs)
	}
	record := logs[0]
	if record["msg"] != "模型切换" {
		t.Errorf("日志 msg = %v", record["msg"])
	}
	if record["level"] != "WARN" {
		t.Errorf("日志 level = %v", record["level"])
	}
	if record["from"] != "primary" || record["to"] != "backup" {
		t.Errorf("日志 from/to = %v/%v", record["from"], record["to"])
	}
	if record["kind"] != string(KindTransient) {
		t.Errorf("日志 kind = %v，期望 %q", record["kind"], KindTransient)
	}
	if reason, _ := record["reason"].(string); !strings.Contains(reason, "HTTP 500") {
		t.Errorf("日志 reason = %v，期望包含 HTTP 500", record["reason"])
	}
}

func TestRouterSwitchesAcrossMultipleProfiles(t *testing.T) {
	var c1, c2, c3 int32
	first := countingStatusServer(t, &c1, http.StatusTooManyRequests, `{"error":"rate limit"}`)
	second := countingStatusServer(t, &c2, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	third := countingSuccessServer(t, &c3, "第三个 profile 的答案")
	logger, buf := newJSONLogger()

	router := newTestRouter(t, []Profile{
		{Name: "primary", BaseURL: first.URL, Model: "m"},
		{Name: "secondary", BaseURL: second.URL, Model: "m"},
		{Name: "tertiary", BaseURL: third.URL, Model: "m"},
	}, 1, logger)

	resp, err := router.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete：%v", err)
	}
	if resp.Content != "第三个 profile 的答案" {
		t.Errorf("Content = %q", resp.Content)
	}
	if got := atomic.LoadInt32(&c1); got == 0 {
		t.Error("第一个 profile 未被尝试")
	}
	if got := atomic.LoadInt32(&c2); got != 1 {
		t.Errorf("第二个 profile 请求次数 = %d，期望 1", got)
	}
	if got := atomic.LoadInt32(&c3); got != 1 {
		t.Errorf("第三个 profile 请求次数 = %d，期望 1", got)
	}

	logs := parseRouterLogs(t, buf)
	if len(logs) != 2 {
		t.Fatalf("切换日志条数 = %d，期望 2：%v", len(logs), logs)
	}
	if logs[0]["from"] != "primary" || logs[0]["to"] != "secondary" || logs[0]["kind"] != string(KindRateLimit) {
		t.Errorf("第一条切换日志 = %v", logs[0])
	}
	if logs[1]["from"] != "secondary" || logs[1]["to"] != "tertiary" || logs[1]["kind"] != string(KindAuth) {
		t.Errorf("第二条切换日志 = %v", logs[1])
	}
}

func TestRouterAllProfilesFail(t *testing.T) {
	var c1, c2, c3 int32
	const key1 = "sk-router-secret-one"
	first := countingStatusServer(t, &c1, http.StatusInternalServerError, "server error")
	second := countingStatusServer(t, &c2, http.StatusUnauthorized, `{"error":"invalid key sk-router-secret-two"}`)
	third := countingStatusServer(t, &c3, http.StatusTooManyRequests, `{"error":"rate limit"}`)
	logger, buf := newJSONLogger()

	router := newTestRouter(t, []Profile{
		{Name: "primary", BaseURL: first.URL, Model: "m", APIKey: key1},
		{Name: "secondary", BaseURL: second.URL, Model: "m", APIKey: "sk-router-secret-two"},
		{Name: "tertiary", BaseURL: third.URL, Model: "m", APIKey: "sk-router-secret-three"},
	}, 1, logger)

	_, err := router.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("期望全部失败后报错")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As 未取到最后的 *APIError：%v", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("最后错误状态码 = %d，期望 429", apiErr.StatusCode)
	}
	if got := ClassifyError(err); got != KindRateLimit {
		t.Errorf("ClassifyError = %q，期望 %q", got, KindRateLimit)
	}
	if !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "profile") {
		t.Errorf("错误文本应包含尝试过的 profile 数量：%q", err.Error())
	}
	for _, key := range []string{key1, "sk-router-secret-two", "sk-router-secret-three"} {
		if strings.Contains(err.Error(), key) {
			t.Errorf("错误文本泄漏密钥 %q：%q", key, err.Error())
		}
	}

	logs := parseRouterLogs(t, buf)
	if len(logs) != 2 {
		t.Errorf("切换日志条数 = %d，期望 2", len(logs))
	}
}

func TestRouterDoesNotSwitchOnContextOverflow(t *testing.T) {
	var firstCalls, secondCalls int32
	first := countingStatusServer(t, &firstCalls, http.StatusBadRequest,
		`{"error":{"message":"This model's maximum context length is 8192 tokens"}}`)
	second := countingSuccessServer(t, &secondCalls, "不应被调用")
	logger, buf := newJSONLogger()

	router := newTestRouter(t, []Profile{
		{Name: "primary", BaseURL: first.URL, Model: "m"},
		{Name: "backup", BaseURL: second.URL, Model: "m"},
	}, 1, logger)

	_, err := router.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("期望上下文超限错误")
	}
	if got := ClassifyError(err); got != KindContextOverflow {
		t.Errorf("ClassifyError = %q，期望 %q", got, KindContextOverflow)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("错误 = %v，期望 *APIError(400)", err)
	}
	if got := atomic.LoadInt32(&secondCalls); got != 0 {
		t.Errorf("context_overflow 不应切换，第二个 profile 请求次数 = %d", got)
	}
	if got := atomic.LoadInt32(&firstCalls); got != 1 {
		t.Errorf("第一个 profile 请求次数 = %d，期望 1（不重试）", got)
	}
	if logs := parseRouterLogs(t, buf); len(logs) != 0 {
		t.Errorf("不应写切换日志：%v", logs)
	}
}

func TestRouterSwitchesOnAuth(t *testing.T) {
	var c1, c2 int32
	first := countingStatusServer(t, &c1, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	second := countingSuccessServer(t, &c2, "鉴权失败后切换成功")
	logger, buf := newJSONLogger()

	router := newTestRouter(t, []Profile{
		{Name: "primary", BaseURL: first.URL, Model: "m", APIKey: "bad-key"},
		{Name: "backup", BaseURL: second.URL, Model: "m", APIKey: "good-key"},
	}, 1, logger)

	resp, err := router.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete：%v", err)
	}
	if resp.Content != "鉴权失败后切换成功" {
		t.Errorf("Content = %q", resp.Content)
	}
	logs := parseRouterLogs(t, buf)
	if len(logs) != 1 || logs[0]["kind"] != string(KindAuth) {
		t.Errorf("切换日志 = %v，期望 1 条 kind=auth", logs)
	}
}

func TestRouterSwitchesOnNetworkError(t *testing.T) {
	var c2 int32
	backup := countingSuccessServer(t, &c2, "网络错误后切换成功")
	logger, buf := newJSONLogger()

	// 127.0.0.1:1 上没有监听者，连接会立即被拒绝（离线、无外部网络）。
	router, err := NewRouter([]Profile{
		{Name: "primary", BaseURL: "http://127.0.0.1:1", Model: "m"},
		{Name: "backup", BaseURL: backup.URL, Model: "m"},
	}, 2*time.Second, 1, logger)
	if err != nil {
		t.Fatalf("NewRouter：%v", err)
	}

	resp, err := router.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete：%v", err)
	}
	if resp.Content != "网络错误后切换成功" {
		t.Errorf("Content = %q", resp.Content)
	}
	if atomic.LoadInt32(&c2) != 1 {
		t.Errorf("backup 请求次数 = %d，期望 1", c2)
	}
	logs := parseRouterLogs(t, buf)
	if len(logs) != 1 || logs[0]["kind"] != string(KindTransient) {
		t.Errorf("切换日志 = %v，期望 1 条 kind=transient", logs)
	}
}

func TestNewRouterValidation(t *testing.T) {
	if _, err := NewRouter(nil, time.Second, 1, nil); err == nil || !strings.Contains(err.Error(), "至少") {
		t.Errorf("空 profiles 错误 = %v", err)
	}

	invalidBase := []Profile{{Name: "bad", BaseURL: "://bad", Model: "m"}}
	if _, err := NewRouter(invalidBase, time.Second, 1, nil); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("非法 base_url 错误 = %v", err)
	}

	emptyModel := []Profile{{Name: "no-model", BaseURL: "http://127.0.0.1:1"}}
	if _, err := NewRouter(emptyModel, time.Second, 1, nil); err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("空 model 错误 = %v", err)
	}

	router, err := NewRouter([]Profile{
		{Name: "a", BaseURL: "http://127.0.0.1:1", Model: "m"},
		{BaseURL: "http://127.0.0.1:2", Model: "m"},
	}, time.Second, 1, nil)
	if err != nil {
		t.Fatalf("NewRouter：%v", err)
	}
	names := router.Profiles()
	if !reflect.DeepEqual(names, []string{"a", "profile-2"}) {
		t.Errorf("Profiles() = %v，期望 [a profile-2]", names)
	}
	// 返回值必须是副本：外部修改不能影响 Router。
	names[0] = "mutated"
	if got := router.Profiles()[0]; got != "a" {
		t.Errorf("Profiles() 未返回副本，得到 %q", got)
	}
}

func TestRouterCompleteJSONRoutes(t *testing.T) {
	var c1, c2 int32
	first := countingStatusServer(t, &c1, http.StatusInternalServerError, "server error")
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c2, 1)
		writeChatSuccess(w, "m", `{"ok":true}`, "stop")
	}))
	t.Cleanup(second.Close)

	router := newTestRouter(t, []Profile{
		{Name: "primary", BaseURL: first.URL, Model: "m"},
		{Name: "backup", BaseURL: second.URL, Model: "m"},
	}, 1, nil)

	resp, err := router.CompleteJSON(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("CompleteJSON：%v", err)
	}
	if resp.Content != `{"ok":true}` {
		t.Errorf("Content = %q", resp.Content)
	}
	if atomic.LoadInt32(&c2) != 1 {
		t.Errorf("backup 请求次数 = %d，期望 1", c2)
	}
}

func TestRouterCompleteWithToolsRoutes(t *testing.T) {
	var c1, c2 int32
	first := countingStatusServer(t, &c1, http.StatusTooManyRequests, `{"error":"rate limit"}`)

	var receivedTools int
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c2, 1)
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Tools []any `json:"tools"`
		}
		_ = json.Unmarshal(raw, &body)
		receivedTools = len(body.Tools)
		writeJSON(w, toolResponseBody("", []map[string]any{
			{"id": "call_1", "name": "get_weather", "arguments": `{"city":"北京"}`},
		}))
	}))
	t.Cleanup(second.Close)

	router := newTestRouter(t, []Profile{
		{Name: "primary", BaseURL: first.URL, Model: "m"},
		{Name: "backup", BaseURL: second.URL, Model: "m"},
	}, 1, nil)

	messages := []Message{
		{Role: "user", Content: "北京天气"},
		{Role: "assistant", Content: "查一下", ToolCalls: []ToolCall{{ID: "call_0", Name: "get_weather", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "call_0", Content: `{"temp":20}`},
	}
	originalMessages := append([]Message(nil), messages...)
	tools := []ToolDef{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}
	originalTools := append([]ToolDef(nil), tools...)

	resp, err := router.CompleteWithTools(context.Background(), messages, tools, "auto")
	if err != nil {
		t.Fatalf("CompleteWithTools：%v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "get_weather" {
		t.Errorf("ToolCalls = %+v", resp.ToolCalls)
	}
	if receivedTools != 1 {
		t.Errorf("backup 收到的 tools 数量 = %d，期望 1", receivedTools)
	}
	if !reflect.DeepEqual(messages, originalMessages) {
		t.Errorf("Router 修改了传入的 messages：%+v", messages)
	}
	if !reflect.DeepEqual(tools, originalTools) {
		t.Errorf("Router 修改了传入的 tools：%+v", tools)
	}
}

func TestRouterNilLogger(t *testing.T) {
	var c1, c2 int32
	first := countingStatusServer(t, &c1, http.StatusInternalServerError, "server error")
	second := countingSuccessServer(t, &c2, "成功")
	router := newTestRouter(t, []Profile{
		{Name: "primary", BaseURL: first.URL, Model: "m"},
		{Name: "backup", BaseURL: second.URL, Model: "m"},
	}, 1, nil)

	resp, err := router.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil || resp.Content != "成功" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestRouterSingleProfileFailure(t *testing.T) {
	var calls int32
	srv := countingStatusServer(t, &calls, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	logger, buf := newJSONLogger()
	router := newTestRouter(t, []Profile{{Name: "only", BaseURL: srv.URL, Model: "m"}}, 1, logger)

	_, err := router.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var routeErr *RouteError
	if !errors.As(err, &routeErr) {
		t.Fatalf("错误类型 = %T，期望 *RouteError：%v", err, err)
	}
	if routeErr.Attempts != 1 || routeErr.Kind != KindAuth {
		t.Errorf("RouteError = %+v", routeErr)
	}
	if got := ClassifyError(err); got != KindAuth {
		t.Errorf("ClassifyError = %q，期望 %q", got, KindAuth)
	}
	if logs := parseRouterLogs(t, buf); len(logs) != 0 {
		t.Errorf("没有备用 profile 时不应写切换日志：%v", logs)
	}
}

func TestRouterConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []Message `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		content := ""
		if len(body.Messages) > 0 {
			content = body.Messages[0].Content
		}
		writeChatSuccess(w, "m", content, "stop")
	}))
	t.Cleanup(srv.Close)

	router := newTestRouter(t, []Profile{{Name: "only", BaseURL: srv.URL, Model: "m"}}, 1, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		want := "并发请求"
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := router.Complete(context.Background(), []Message{{Role: "user", Content: want}})
			if err != nil {
				t.Errorf("并发 Complete：%v", err)
				return
			}
			if resp.Content != want {
				t.Errorf("并发响应串扰：%q != %q", resp.Content, want)
			}
		}()
	}
	wg.Wait()
}
