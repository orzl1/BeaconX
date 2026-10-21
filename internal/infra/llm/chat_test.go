package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// writeJSON 把任意值写为响应体；测试处理器里刻意不调用 t.Fatal（不同 goroutine）。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := json.Marshal(v)
	_, _ = w.Write(body)
}

// writeChatSuccess 写入一个包含 content/usage/finish_reason 的成功响应。
func writeChatSuccess(w http.ResponseWriter, model, content, finish string) {
	writeJSON(w, map[string]any{
		"model": model,
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     11,
			"completion_tokens": 7,
			"total_tokens":      18,
		},
	})
}

// mustNewClient 构造测试客户端，失败直接终止当前测试。
func mustNewClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient：%v", err)
	}
	return c
}

func TestCompleteRequestShape(t *testing.T) {
	type captured struct {
		method     string
		path       string
		auth       string
		contentTyp string
		body       map[string]any
	}
	got := make(chan captured, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got <- captured{
			method:     r.Method,
			path:       r.URL.Path,
			auth:       r.Header.Get("Authorization"),
			contentTyp: r.Header.Get("Content-Type"),
			body:       body,
		}
		writeChatSuccess(w, "m", "完成", "stop")
	}))
	defer srv.Close()

	temperature := 0.7
	const apiKey = "sk-test-key-123"
	c := mustNewClient(t, Config{
		BaseURL:     srv.URL + "/v1",
		APIKey:      apiKey,
		Model:       "m",
		MaxTokens:   123,
		Temperature: &temperature,
	})

	messages := []Message{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "你好"},
	}
	if _, err := c.Complete(context.Background(), messages); err != nil {
		t.Fatalf("Complete：%v", err)
	}

	req := <-got
	if req.method != http.MethodPost {
		t.Errorf("请求方法 = %q，期望 POST", req.method)
	}
	if req.path != "/v1/chat/completions" {
		t.Errorf("请求路径 = %q，期望 /v1/chat/completions", req.path)
	}
	if req.auth != "Bearer "+apiKey {
		t.Errorf("Authorization = %q", req.auth)
	}
	if !strings.HasPrefix(req.contentTyp, "application/json") {
		t.Errorf("Content-Type = %q", req.contentTyp)
	}
	if req.body["model"] != "m" {
		t.Errorf("请求体 model = %v", req.body["model"])
	}
	if req.body["max_tokens"] != float64(123) {
		t.Errorf("请求体 max_tokens = %v", req.body["max_tokens"])
	}
	if req.body["temperature"] != 0.7 {
		t.Errorf("请求体 temperature = %v", req.body["temperature"])
	}
	if _, ok := req.body["response_format"]; ok {
		t.Errorf("普通请求不应携带 response_format：%v", req.body["response_format"])
	}
	rawMessages, ok := req.body["messages"].([]any)
	if !ok || len(rawMessages) != 2 {
		t.Fatalf("请求体 messages = %v", req.body["messages"])
	}
	first, _ := rawMessages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手" {
		t.Errorf("第一条消息 = %v", first)
	}
}

func TestCompleteDefaults(t *testing.T) {
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		auth = r.Header.Get("Authorization")
		writeChatSuccess(w, "m", "ok", "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	if c.maxTokens != defaultMaxTokens {
		t.Errorf("默认 maxTokens = %d，期望 %d", c.maxTokens, defaultMaxTokens)
	}
	if c.maxRetries != defaultMaxRetries {
		t.Errorf("默认 maxRetries = %d，期望 %d", c.maxRetries, defaultMaxRetries)
	}
	if c.http.Timeout != defaultTimeout {
		t.Errorf("默认 Timeout = %v，期望 %v", c.http.Timeout, defaultTimeout)
	}

	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete：%v", err)
	}
	if body["max_tokens"] != float64(defaultMaxTokens) {
		t.Errorf("请求体 max_tokens = %v，期望 %d", body["max_tokens"], defaultMaxTokens)
	}
	if _, ok := body["temperature"]; ok {
		t.Errorf("Temperature 为 nil 时不应发送该字段：%v", body["temperature"])
	}
	if auth != "" {
		t.Errorf("APIKey 为空时不应发送 Authorization，得到 %q", auth)
	}
}

func TestBaseURLTrailingSlash(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeChatSuccess(w, "m", "ok", "stop")
	}))
	defer srv.Close()

	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "///"} {
		c := mustNewClient(t, Config{BaseURL: base, Model: "m"})
		if c.baseURL != srv.URL {
			t.Errorf("baseURL %q 归一化为 %q，期望 %q", base, c.baseURL, srv.URL)
		}
		if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
			t.Fatalf("base=%q Complete：%v", base, err)
		}
		if path != "/chat/completions" {
			t.Errorf("base=%q 请求路径 = %q", base, path)
		}
	}
}

func TestCompleteParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeChatSuccess(w, "deepseek-chat", "最终答案", "length")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	resp, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete：%v", err)
	}
	if resp.Content != "最终答案" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.Model != "deepseek-chat" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.FinishReason != "length" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	want := Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}
	if resp.Usage != want {
		t.Errorf("Usage = %+v，期望 %+v", resp.Usage, want)
	}
}

func TestCompleteRejectsMalformedResponses(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{name: "空 choices", body: `{"choices":[]}`, wantSub: "choices"},
		{name: "缺少 content", body: `{"choices":[{"message":{"role":"assistant"}}]}`, wantSub: "内容为空"},
		{name: "空白 content", body: `{"choices":[{"message":{"content":"   "}}]}`, wantSub: "内容为空"},
		{name: "非法 JSON", body: `not-json`, wantSub: "解析"},
		{name: "JSON 类型不符", body: `[]`, wantSub: "解析"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
			_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
			if err == nil {
				t.Fatalf("期望报错，实际成功")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误 = %q，期望包含 %q", err.Error(), tc.wantSub)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Errorf("解析类错误不应重试，请求次数 = %d", got)
			}
		})
	}
}

func TestResponseBodyTooLarge(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxBodyBytes+1024))
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超限响应应报错，实际 err = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("超限错误不应重试，请求次数 = %d", got)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeChatSuccess(w, "m", "ok", "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	huge := strings.Repeat("x", maxBodyBytes+1024)
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: huge}})
	if err == nil || !strings.Contains(err.Error(), "请求体超过") {
		t.Fatalf("超限请求应报错，实际 err = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("请求体超限时不应发出请求，请求次数 = %d", got)
	}
}

func TestAPIErrorRedactsAPIKey(t *testing.T) {
	const apiKey = "sk-secret-should-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{
			"error": map[string]any{"message": "invalid api key " + apiKey},
		})
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m", APIKey: apiKey})
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("期望 401 报错")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("错误文本泄漏了 API Key：%q", err.Error())
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型 = %T，期望 *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	if strings.Contains(apiErr.Body, apiKey) {
		t.Errorf("APIError.Body 泄漏了 API Key：%q", apiErr.Body)
	}
	if !strings.Contains(apiErr.Body, "[已隐藏]") {
		t.Errorf("APIError.Body 未体现脱敏：%q", apiErr.Body)
	}
}

func TestAPIErrorTruncatesLongBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, strings.Repeat("x", errorBodyLimit*2))
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型 = %T，期望 *APIError", err)
	}
	if len(apiErr.Body) > errorBodyLimit+len("…（已截断）") {
		t.Errorf("APIError.Body 过长（%d 字节）", len(apiErr.Body))
	}
	if !strings.HasSuffix(apiErr.Body, "…（已截断）") {
		t.Errorf("APIError.Body 缺少截断标记：%q", apiErr.Body)
	}
}

func TestConcurrentComplete(t *testing.T) {
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
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("请求-%d", i)
			resp, err := c.Complete(context.Background(), []Message{{Role: "user", Content: want}})
			if err != nil {
				t.Errorf("并发 Complete(%d)：%v", i, err)
				return
			}
			if resp.Content != want {
				t.Errorf("并发响应串扰：得到 %q，期望 %q", resp.Content, want)
			}
		}()
	}
	wg.Wait()
}
