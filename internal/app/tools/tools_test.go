package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestRegistryVisibilityAndValidation(t *testing.T) {
	reg, err := NewRegistry(NewHTTPProbe(), NewShellExec())
	if err != nil {
		t.Fatalf("创建注册表失败: %v", err)
	}
	if _, ok := reg.Get("http_probe"); !ok {
		t.Fatal("缺少 http_probe")
	}
	if len(reg.List("worker")) != 2 {
		t.Fatalf("worker 可见工具数 = %d", len(reg.List("worker")))
	}
	if len(reg.List("planner")) != 0 {
		t.Fatalf("planner 不应看到 M4 工具")
	}
	if _, err := NewRegistry(NewHTTPProbe(), NewHTTPProbe()); err == nil {
		t.Fatal("重复注册应报错")
	}
}

func TestToolSchemasAreValidJSON(t *testing.T) {
	for _, tool := range []Tool{NewHTTPProbe(), NewShellExec()} {
		def := tool.Definition()
		if !json.Valid(def.Parameters) {
			t.Fatalf("%s schema 不是合法 JSON", def.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(def.Parameters, &schema); err != nil {
			t.Fatalf("%s schema 解析失败: %v", def.Name, err)
		}
		if schema["type"] != "object" {
			t.Fatalf("%s schema 顶层类型不是 object", def.Name)
		}
		req, _ := schema["required"].([]any)
		if len(req) == 0 {
			t.Fatalf("%s schema 缺少 required", def.Name)
		}
	}
}

func TestHTTPProbeExecute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello beaconx"))
	}))
	defer srv.Close()

	res, err := NewHTTPProbe().Execute(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(res.Output, "HTTP 418") || !strings.Contains(res.Output, "hello beaconx") {
		t.Fatalf("输出不符合预期: %s", res.Output)
	}
	if res.Meta["status"] != http.StatusTeapot {
		t.Fatalf("状态码元数据 = %v", res.Meta["status"])
	}
}

func TestHTTPProbeRejectsNonHTTP(t *testing.T) {
	if _, err := NewHTTPProbe().Execute(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`)); err == nil {
		t.Fatal("非 http(s) URL 应被拒绝")
	}
}

func TestShellExecExecute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("命令语法按 Unix 编写")
	}
	res, err := NewShellExec().Execute(context.Background(), json.RawMessage(`{"command":"printf beaconx-ok"}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(res.Output, "beaconx-ok") {
		t.Fatalf("输出不符合预期: %s", res.Output)
	}
}

func TestShellExecFailureIncludesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("命令语法按 Unix 编写")
	}
	_, err := NewShellExec().Execute(context.Background(), json.RawMessage(`{"command":"echo boom && exit 3"}`))
	if err == nil {
		t.Fatal("非零退出码应报错")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("错误应包含命令输出: %v", err)
	}
}
