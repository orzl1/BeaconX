package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// timeoutError 是一个满足 net.Error 的测试用网络错误。
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestErrorKindValues(t *testing.T) {
	cases := map[ErrorKind]string{
		KindAuth:            "auth",
		KindQuota:           "quota",
		KindRateLimit:       "rate_limit",
		KindContextOverflow: "context_overflow",
		KindTransient:       "transient",
		KindFatal:           "fatal",
	}
	for kind, want := range cases {
		if string(kind) != want {
			t.Errorf("ErrorKind 值 = %q，期望 %q", kind, want)
		}
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{name: "nil", err: nil, want: KindFatal},

		{name: "401", err: &APIError{StatusCode: http.StatusUnauthorized}, want: KindAuth},
		{name: "403", err: &APIError{StatusCode: http.StatusForbidden}, want: KindAuth},
		{name: "402", err: &APIError{StatusCode: http.StatusPaymentRequired}, want: KindQuota},
		{name: "429", err: &APIError{StatusCode: http.StatusTooManyRequests}, want: KindRateLimit},
		{name: "500", err: &APIError{StatusCode: http.StatusInternalServerError}, want: KindTransient},
		{name: "502", err: &APIError{StatusCode: http.StatusBadGateway}, want: KindTransient},

		{
			name: "400 上下文超限",
			err:  &APIError{StatusCode: 400, Body: `{"error":"This model's maximum context length is 8192 tokens"}`},
			want: KindContextOverflow,
		},
		{
			name: "400 too many tokens",
			err:  &APIError{StatusCode: 400, Body: "too many tokens in request"},
			want: KindContextOverflow,
		},
		{
			name: "400 prompt too long",
			err:  &APIError{StatusCode: 400, Body: "prompt is too long: 90000 tokens"},
			want: KindContextOverflow,
		},
		{name: "400 其他", err: &APIError{StatusCode: 400, Body: "invalid request"}, want: KindFatal},
		{name: "404", err: &APIError{StatusCode: http.StatusNotFound}, want: KindFatal},
		{name: "408", err: &APIError{StatusCode: http.StatusRequestTimeout}, want: KindFatal},
		{name: "状态码 0", err: &APIError{}, want: KindFatal},

		{
			name: "包装后的 APIError",
			err:  fmt.Errorf("调用失败：%w", &APIError{StatusCode: http.StatusUnauthorized}),
			want: KindAuth,
		},
		{
			name: "transportError",
			err:  fmt.Errorf("请求 LLM 接口失败：%w", &transportError{err: errors.New("connection reset")}),
			want: KindTransient,
		},
		{name: "net.Error", err: timeoutError{}, want: KindTransient},
		{name: "context deadline", err: context.DeadlineExceeded, want: KindTransient},
		{
			name: "包装后的 context deadline",
			err:  fmt.Errorf("请求超时：%w", context.DeadlineExceeded),
			want: KindTransient,
		},
		{name: "context canceled", err: context.Canceled, want: KindFatal},
		{name: "未知错误", err: errors.New("boom"), want: KindFatal},
		{
			name: "RouteError 使用记录类别",
			err:  &RouteError{Attempts: 2, Kind: KindQuota, Last: &APIError{StatusCode: http.StatusInternalServerError}},
			want: KindQuota,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Errorf("ClassifyError(%v) = %q，期望 %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestAPIErrorKindMethod(t *testing.T) {
	if got := (&APIError{StatusCode: 400, Body: "context length exceeded"}).Kind(); got != KindContextOverflow {
		t.Errorf("Kind() = %q，期望 %q", got, KindContextOverflow)
	}
	if got := (&APIError{StatusCode: 429}).Kind(); got != KindRateLimit {
		t.Errorf("Kind() = %q，期望 %q", got, KindRateLimit)
	}
	var nilErr *APIError
	if got := nilErr.Kind(); got != KindFatal {
		t.Errorf("nil APIError.Kind() = %q，期望 %q", got, KindFatal)
	}
}

func TestClientAPIErrorKindEndToEnd(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrorKind
	}{
		{
			name:   "400 上下文超限",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"maximum context length exceeded"}}`,
			want:   KindContextOverflow,
		},
		{name: "403 权限", status: http.StatusForbidden, body: `{"error":"forbidden"}`, want: KindAuth},
		{name: "400 其他", status: http.StatusBadRequest, body: `{"error":"bad request"}`, want: KindFatal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
			_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
			if err == nil {
				t.Fatal("期望报错")
			}
			if got := ClassifyError(err); got != tc.want {
				t.Errorf("ClassifyError = %q，期望 %q", got, tc.want)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("错误类型 = %T，期望 *APIError", err)
			}
			if got := apiErr.Kind(); got != tc.want {
				t.Errorf("APIError.Kind() = %q，期望 %q", got, tc.want)
			}
			// 分类不得改变既有错误文本。
			if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tc.status)) {
				t.Errorf("错误文本 = %q，期望仍包含 HTTP 状态码", err.Error())
			}
		})
	}
}
