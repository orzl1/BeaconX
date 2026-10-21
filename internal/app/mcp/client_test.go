package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件是 internal/app/mcp 的离线单测：不依赖数据库与外部网络。
// HTTP 场景用 httptest，stdio 只覆盖安全策略（不真实执行命令）。

// mockMCP 是支持 JSON/SSE 两种响应形态的最小 MCP 服务。
type mockMCP struct {
	tools    []map[string]any
	sse      bool
	failInit bool
}

func (m *mockMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     *int   `json:"id"`
		Method string `json:"method"`
	}
	body := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	switch req.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "initialize":
		if m.failInit {
			m.write(w, map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32000, "message": "拒绝握手"}})
			return
		}
		m.write(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{},
		}})
	case "tools/list":
		m.write(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": m.tools}})
	default:
		m.write(w, map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}

func (m *mockMCP) write(w http.ResponseWriter, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	if m.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// TestDiscoverHTTPJSON 覆盖 initialize + tools/list 的 JSON 路径与 schema 解析。
func TestDiscoverHTTPJSON(t *testing.T) {
	mock := &mockMCP{tools: []map[string]any{
		{"name": "alpha", "description": "A", "inputSchema": map[string]any{"type": "object"}},
		{"name": "beta", "description": "", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}},
	}}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	tools, err := Discover(context.Background(), Server{
		Name: "mock", Transport: TransportHTTP, URL: srv.URL,
		Headers: map[string]string{"X-Test": "1"},
	}, Options{})
	if err != nil {
		t.Fatalf("发现失败: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "alpha" || tools[1].Name != "beta" {
		t.Fatalf("工具清单 = %+v", tools)
	}
	if tools[0].InputSchema["type"] != "object" {
		t.Fatalf("inputSchema 未解析: %+v", tools[0].InputSchema)
	}
}

// TestDiscoverHTTPSSE 覆盖 text/event-stream 响应。
func TestDiscoverHTTPSSE(t *testing.T) {
	mock := &mockMCP{sse: true, tools: []map[string]any{{"name": "sse-tool"}}}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	tools, err := Discover(context.Background(), Server{Name: "sse", Transport: TransportHTTP, URL: srv.URL}, Options{})
	if err != nil {
		t.Fatalf("SSE 发现失败: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "sse-tool" {
		t.Fatalf("SSE 工具清单 = %+v", tools)
	}
}

// TestDiscoverHTTPProtocolError 覆盖 initialize 被拒绝。
func TestDiscoverHTTPProtocolError(t *testing.T) {
	mock := &mockMCP{failInit: true}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	_, err := Discover(context.Background(), Server{Name: "bad", Transport: TransportHTTP, URL: srv.URL}, Options{})
	if err == nil || !strings.Contains(err.Error(), "被拒绝") {
		t.Fatalf("期望握手被拒绝的错误，实际: %v", err)
	}
}

// TestDiscoverHTTPStatusError 覆盖非 2xx 状态。
func TestDiscoverHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := Discover(context.Background(), Server{Name: "500", Transport: TransportHTTP, URL: srv.URL}, Options{})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("期望包含状态码的错误，实际: %v", err)
	}
}

// TestDiscoverURLValidation 覆盖 url 缺失/非 http(s)。
func TestDiscoverURLValidation(t *testing.T) {
	for _, url := range []string{"", "ftp://example.com", "file:///etc/passwd"} {
		if _, err := Discover(context.Background(), Server{Name: "x", Transport: TransportHTTP, URL: url}, Options{}); err == nil {
			t.Fatalf("url=%q 期望报错", url)
		}
	}
}

// TestDiscoverStdioDisabled 覆盖 stdio 默认禁用。
func TestDiscoverStdioDisabled(t *testing.T) {
	_, err := Discover(context.Background(), Server{Name: "std", Transport: TransportStdio, Command: "echo"}, Options{})
	if !errors.Is(err, ErrStdioDisabled) {
		t.Fatalf("期望 ErrStdioDisabled，实际: %v", err)
	}
}

// TestDiscoverStdioAllowlist 覆盖命令白名单拒绝（不执行进程）。
func TestDiscoverStdioAllowlist(t *testing.T) {
	_, err := Discover(context.Background(), Server{Name: "std", Transport: TransportStdio, Command: "echo"}, Options{
		AllowStdio:     true,
		StdioAllowlist: []string{"/usr/local/bin/other"},
	})
	if !errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("期望 ErrCommandNotAllowed，实际: %v", err)
	}
}

// TestDiscoverToolCap 覆盖工具数量上限。
func TestDiscoverToolCap(t *testing.T) {
	tools := make([]map[string]any, 0, maxTools+50)
	for i := 0; i < maxTools+50; i++ {
		tools = append(tools, map[string]any{"name": fmt.Sprintf("tool-%03d", i)})
	}
	mock := &mockMCP{tools: tools}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	got, err := Discover(context.Background(), Server{Name: "many", Transport: TransportHTTP, URL: srv.URL}, Options{})
	if err != nil {
		t.Fatalf("发现失败: %v", err)
	}
	if len(got) != maxTools {
		t.Fatalf("工具数 = %d，期望上限 %d", len(got), maxTools)
	}
}

// TestDiscoverTimeout 覆盖超时（服务器故意不响应）。
func TestDiscoverTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	_, err := Discover(context.Background(), Server{Name: "slow", Transport: TransportHTTP, URL: srv.URL},
		Options{Timeout: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("期望超时错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未生效，耗时 %s", elapsed)
	}
}
