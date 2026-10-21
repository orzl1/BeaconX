package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orzl1/BeaconX/internal/tenant"
)

func testHandler(t *testing.T, opts Options) http.Handler {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h, err := NewHandler(opts)
	if err != nil {
		t.Fatalf("NewHandler：%v", err)
	}
	return h
}

func TestRoutesUniqueAndBound(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range Routes() {
		key := rt.Method + " " + rt.Pattern
		if seen[key] {
			t.Fatalf("重复的路由 %s", key)
		}
		seen[key] = true
		if rt.Summary == "" {
			t.Errorf("路由 %s 缺少摘要", key)
		}
		if _, err := (Options{}).bind(rt); err != nil {
			t.Errorf("路由 %s 未绑定处理器：%v", key, err)
		}
	}
	if _, err := (Options{}).bind(Route{Method: http.MethodGet, Pattern: "/nope"}); err == nil {
		t.Fatal("未绑定的路由必须被拒绝")
	}
}

func TestHealthz(t *testing.T) {
	h := testHandler(t, Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("响应体 = %v", body)
	}
}

func TestRequestIDHeaderIsPreserved(t *testing.T) {
	h := testHandler(t, Options{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "abc123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "abc123" {
		t.Fatalf("X-Request-ID = %q", got)
	}
}

func TestRequestIDIsGeneratedWhenMissing(t *testing.T) {
	h := testHandler(t, Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("缺失时必须生成 X-Request-ID")
	}
}

func TestTenantContext(t *testing.T) {
	h := testHandler(t, Options{TenantResolver: StaticTenant(tenant.ID(42))})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/tenant", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var body map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["tenant_id"] != 42 {
		t.Fatalf("tenant_id = %d", body["tenant_id"])
	}
}

func TestTenantFallsBackToDefault(t *testing.T) {
	h := testHandler(t, Options{TenantResolver: StaticTenant(0)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/tenant", nil))
	var body map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["tenant_id"] != int64(tenant.DefaultID) {
		t.Fatalf("tenant_id = %d", body["tenant_id"])
	}
}

func TestReadyzStatusMapping(t *testing.T) {
	t.Run("就绪", func(t *testing.T) {
		h := testHandler(t, Options{
			Ready: func(context.Context) (any, bool) {
				return map[string]any{"ready": true}, true
			},
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d", rec.Code)
		}
	})
	t.Run("未就绪", func(t *testing.T) {
		h := testHandler(t, Options{
			Ready: func(context.Context) (any, bool) {
				return map[string]any{"ready": false, "checks": []any{}}, false
			},
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("状态码 = %d", rec.Code)
		}
	})
}

func TestInfoPayload(t *testing.T) {
	h := testHandler(t, Options{
		Info: func(context.Context) any { return map[string]string{"service": "beaconx"} },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "beaconx") {
		t.Fatalf("响应体 = %s", rec.Body.String())
	}
}

func TestProtectedRouteFailsClosedWithoutAuthenticator(t *testing.T) {
	opts := Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := opts.withAuth(Route{Method: http.MethodGet, Pattern: "/protected"}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/protected", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "auth_unavailable") {
		t.Fatalf("响应体 = %s", rec.Body.String())
	}
}

func TestRecoverReturnsJSON(t *testing.T) {
	opts := Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := opts.recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("模拟 panic")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Fatalf("响应体 = %s", rec.Body.String())
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	h := testHandler(t, Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d", rec.Code)
	}
}
