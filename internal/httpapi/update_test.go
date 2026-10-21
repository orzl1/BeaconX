package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/orzl1/BeaconX/internal/infra/selfupdate"
)

func TestUpdateCheckAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("构造路径按 Unix")
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v9.9.9", "html_url": "https://example.com/release", "body": "新版本",
			"assets": []map[string]string{
				{"name": "beaconx-" + runtime.GOOS + "-" + runtime.GOARCH + ".zip", "browser_download_url": srv.URL + "/a.zip"},
				{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/s"},
			},
		})
	}))
	defer srv.Close()

	bin := filepath.Join(t.TempDir(), "beaconx")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho beaconx 1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := selfupdate.New(selfupdate.Config{
		Repo: "o/r", CurrentVersion: "1.0.0", BinaryPath: bin,
		StateDir: filepath.Join(t.TempDir(), "state"), Enabled: true,
		HTTPClient: srv.Client(), APIBase: srv.URL,
	})
	h, err := NewHandler(Options{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		TenantID: 1, Updater: updater, Authenticator: NewTokenAuthenticator("t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/update/check", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("check status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		HasUpdate bool               `json:"has_update"`
		Release   selfupdate.Release `json:"release"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.HasUpdate || body.Release.Version != "9.9.9" {
		t.Fatalf("检查结果不符合预期: %+v", body)
	}
}
