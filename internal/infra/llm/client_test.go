package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewClientValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "空 base_url", cfg: Config{Model: "m"}, wantErr: "base_url"},
		{name: "无法解析", cfg: Config{BaseURL: "://bad", Model: "m"}, wantErr: "base_url"},
		{name: "缺少协议", cfg: Config{BaseURL: "api.example.com", Model: "m"}, wantErr: "http"},
		{name: "协议不支持", cfg: Config{BaseURL: "ftp://api.example.com", Model: "m"}, wantErr: "http"},
		{name: "缺少主机", cfg: Config{BaseURL: "http://", Model: "m"}, wantErr: "主机名"},
		{name: "空 model", cfg: Config{BaseURL: "http://127.0.0.1:1"}, wantErr: "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v，期望包含 %q", err, tc.wantErr)
			}
		})
	}

	if _, err := NewClient(Config{BaseURL: "http://127.0.0.1:1", Model: "m"}); err != nil {
		t.Fatalf("APIKey 允许为空，却报错：%v", err)
	}
}

func TestCompleteRetriesOn500ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "临时故障")
			return
		}
		writeChatSuccess(w, "m", "重试成功", "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m", MaxRetries: 2})
	resp, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete：%v", err)
	}
	if resp.Content != "重试成功" {
		t.Errorf("Content = %q", resp.Content)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("请求次数 = %d，期望 2（1 次失败 + 1 次成功）", got)
	}
}

func TestCompleteStopsAfter429Retries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limit"}`)
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m", MaxRetries: 2})
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("期望重试耗尽后报错")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("错误 = %v，期望 *APIError(429)", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("请求次数 = %d，期望 3（MaxRetries=2）", got)
	}
}

func TestCompleteDoesNotRetryOn401(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m", MaxRetries: 3})
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 = %v，期望 *APIError(401)", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("4xx 不应重试，请求次数 = %d", got)
	}
}

func TestCompleteJSONSendsResponseFormat(t *testing.T) {
	var body map[string]any
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeChatSuccess(w, "m", `{"answer":42}`, "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	resp, err := c.CompleteJSON(context.Background(), []Message{{Role: "user", Content: "给出 JSON"}})
	if err != nil {
		t.Fatalf("CompleteJSON：%v", err)
	}
	if resp.Content != `{"answer":42}` {
		t.Errorf("Content = %q", resp.Content)
	}
	format, ok := body["response_format"].(map[string]any)
	if !ok || format["type"] != "json_object" {
		t.Errorf("response_format = %v", body["response_format"])
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("JSON 模式成功时请求次数 = %d，期望 1", got)
	}
}

func TestCompleteJSONFallsBackOn400(t *testing.T) {
	var calls int32
	var sawResponseFormat []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		_, hasFormat := body["response_format"]
		sawResponseFormat = append(sawResponseFormat, hasFormat)
		if hasFormat {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"response_format unsupported"}`)
			return
		}
		writeChatSuccess(w, "m", `{"fallback":true}`, "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	resp, err := c.CompleteJSON(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("CompleteJSON 回退后应成功：%v", err)
	}
	if resp.Content != `{"fallback":true}` {
		t.Errorf("Content = %q", resp.Content)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("请求次数 = %d，期望 2（JSON 被拒 + 普通回退）", got)
	}
	if len(sawResponseFormat) != 2 || !sawResponseFormat[0] || sawResponseFormat[1] {
		t.Errorf("两次请求的 response_format 情况 = %v，期望 [true false]", sawResponseFormat)
	}
}

func TestCompleteJSONDoesNotFallBackOn500(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "server error")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m", MaxRetries: 1})
	_, err := c.CompleteJSON(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("错误 = %v，期望 *APIError(500)", err)
	}
	// 500 只走 JSON 模式自身的重试（1 次重试 = 共 2 次），不应触发 400 回退。
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("请求次数 = %d，期望 2（不触发回退）", got)
	}
}

func TestCompleteTimesOut(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
			writeChatSuccess(w, "m", "迟到的响应", "stop")
		}
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{
		BaseURL:    srv.URL,
		Model:      "m",
		Timeout:    50 * time.Millisecond,
		MaxRetries: 1,
	})
	start := time.Now()
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("期望超时错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("超时未生效，耗时 %v", elapsed)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("超时不应被归类为 *APIError：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 1 || got > 2 {
		t.Errorf("请求次数 = %d，期望 1~2", got)
	}
}

func TestContextCanceledDuringBackoff(t *testing.T) {
	var calls int32
	firstCall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(firstCall)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "server error")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-firstCall
		cancel()
	}()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m", MaxRetries: 3})
	_, err := c.Complete(ctx, []Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望包含 context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("取消后不应继续请求，请求次数 = %d", got)
	}
}

func TestParentContextAlreadyCanceled(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeChatSuccess(w, "m", "不应返回", "stop")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "hi"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望包含 context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("已取消的 ctx 不应发出请求，请求次数 = %d", got)
	}
}

func TestCompleteRejectsEmptyMessages(t *testing.T) {
	c := mustNewClient(t, Config{BaseURL: "http://127.0.0.1:1", Model: "m"})
	if _, err := c.Complete(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "messages") {
		t.Fatalf("err = %v，期望提醒 messages 不能为空", err)
	}
}
