package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndexAndAPI404(t *testing.T) {
	h := Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "BeaconX") {
		t.Fatalf("根路径应返回首页: code=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missions/1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("前端路由应回退到首页: code=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("未知 API 应返回 JSON 404: code=%d body=%s", rec.Code, rec.Body.String())
	}
}
