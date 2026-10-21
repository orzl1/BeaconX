package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func makeBin(version string, fail bool) []byte {
	script := "#!/bin/sh\n"
	if fail {
		script += "exit 1\n"
	} else {
		script += fmt.Sprintf("echo \"beaconx %s\"\n", version)
	}
	return []byte(script)
}

func makeZip(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("beaconx")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bin); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCheckApplyRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("脚本化二进制仅适用于 Unix")
	}
	zipData := makeZip(t, makeBin("2.0.0", false))
	sum := sha256.Sum256(zipData)
	sums := fmt.Sprintf("%s  beaconx-%s-%s.zip\n", hex.EncodeToString(sum[:]), runtime.GOOS, runtime.GOARCH)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v2.0.0", "html_url": "https://example.com", "body": "notes",
				"published_at": "2026-01-01T00:00:00Z",
				"assets": []map[string]string{
					{"name": fmt.Sprintf("beaconx-%s-%s.zip", runtime.GOOS, runtime.GOARCH), "browser_download_url": srv.URL + "/asset.zip"},
					{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/sums"},
				},
			})
		case "/asset.zip":
			_, _ = w.Write(zipData)
		case "/sums":
			_, _ = w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	binPath := filepath.Join(dir, "beaconx")
	if err := os.WriteFile(binPath, makeBin("1.0.0", false), 0o755); err != nil {
		t.Fatal(err)
	}
	u := New(Config{Repo: "o/r", CurrentVersion: "1.0.0", BinaryPath: binPath, StateDir: filepath.Join(dir, "state"),
		Enabled: true, HTTPClient: srv.Client(), APIBase: srv.URL, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH})

	rel, has, err := u.Check(context.Background())
	if err != nil || !has {
		t.Fatalf("Check 失败: has=%v err=%v", has, err)
	}
	if rel.Version != "2.0.0" {
		t.Fatalf("版本解析错误: %s", rel.Version)
	}
	if err := u.Apply(context.Background(), rel); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	got, _ := os.ReadFile(binPath)
	if !bytes.Contains(got, []byte("2.0.0")) {
		t.Fatalf("二进制未更新: %s", got)
	}
	if _, err := os.Stat(binPath + ".old"); err != nil {
		t.Fatalf("缺少备份: %v", err)
	}
	if err := u.Rollback(); err != nil {
		t.Fatalf("Rollback 失败: %v", err)
	}
	got, _ = os.ReadFile(binPath)
	if !bytes.Contains(got, []byte("1.0.0")) {
		t.Fatalf("回滚未生效: %s", got)
	}
}

func TestChecksumMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("脚本化二进制仅适用于 Unix")
	}
	zipData := makeZip(t, makeBin("2.0.0", false))
	sums := "deadbeef  beaconx-" + runtime.GOOS + "-" + runtime.GOARCH + ".zip\n"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v2.0.0",
				"assets": []map[string]string{
					{"name": fmt.Sprintf("beaconx-%s-%s.zip", runtime.GOOS, runtime.GOARCH), "browser_download_url": srv.URL + "/a.zip"},
					{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/s"},
				},
			})
		case "/a.zip":
			_, _ = w.Write(zipData)
		case "/s":
			_, _ = w.Write([]byte(sums))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "beaconx")
	_ = os.WriteFile(binPath, makeBin("1.0.0", false), 0o755)
	u := New(Config{Repo: "o/r", CurrentVersion: "1.0.0", BinaryPath: binPath, StateDir: filepath.Join(dir, "s"),
		Enabled: true, HTTPClient: srv.Client(), APIBase: srv.URL})
	rel, has, err := u.Check(context.Background())
	if err != nil || !has {
		t.Fatalf("Check: %v %v", has, err)
	}
	if err := u.Apply(context.Background(), rel); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("期望校验和错误，实际: %v", err)
	}
	got, _ := os.ReadFile(binPath)
	if !bytes.Contains(got, []byte("1.0.0")) {
		t.Fatal("校验失败时不应替换二进制")
	}
}

func TestSmokeFailureDoesNotReplace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("脚本化二进制仅适用于 Unix")
	}
	zipData := makeZip(t, makeBin("2.0.0", true))
	sum := sha256.Sum256(zipData)
	sums := hex.EncodeToString(sum[:]) + "  beaconx-" + runtime.GOOS + "-" + runtime.GOARCH + ".zip\n"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v2.0.0",
				"assets": []map[string]string{
					{"name": fmt.Sprintf("beaconx-%s-%s.zip", runtime.GOOS, runtime.GOARCH), "browser_download_url": srv.URL + "/a.zip"},
					{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/s"},
				},
			})
		case "/a.zip":
			_, _ = w.Write(zipData)
		case "/s":
			_, _ = w.Write([]byte(sums))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "beaconx")
	_ = os.WriteFile(binPath, makeBin("1.0.0", false), 0o755)
	u := New(Config{Repo: "o/r", CurrentVersion: "1.0.0", BinaryPath: binPath, StateDir: filepath.Join(dir, "s"),
		Enabled: true, HTTPClient: srv.Client(), APIBase: srv.URL})
	rel, _, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Apply(context.Background(), rel); !errors.Is(err, ErrSmokeFailed) {
		t.Fatalf("期望冒烟失败，实际: %v", err)
	}
	got, _ := os.ReadFile(binPath)
	if !bytes.Contains(got, []byte("1.0.0")) {
		t.Fatal("冒烟失败时不应替换二进制")
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.0.0", "1.0.0", 1},
		{"1.0.0", "1.0.0", 0},
		{"v1.2.3", "1.2.2", 1},
		{"0.9.0", "1.0.0", -1},
		{"dev", "1.0.0", -1},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Fatalf("compareVersion(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}
