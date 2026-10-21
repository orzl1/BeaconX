package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/tenant"
)

// M6.2-B5-1/B5-2 的端到端测试（P-B5-01..17）。
//
// 复用 insightsHarness（真 PostgreSQL + 租户 A 的 HTTP 服务 + 租户 B），
// 未设置 BEACONX_TEST_PG_DSN 时整体跳过。MCP 的 http 传输用 httptest 起一个
// 最小 JSON-RPC 服务（initialize / notifications/initialized / tools/list），
// 不依赖任何外部网络；stdio 只测「默认禁用」与「命令不可用」两条安全路径，
// 不在测试里执行真实命令。
//
// 辅助函数统一 b5mcp 前缀，避免与同包其它测试文件冲突。

// b5mcpDo 发送请求并断言状态码，返回响应体。
func (h *insightsHarness) b5mcpDo(t *testing.T, method, path, body string, want int) []byte {
	t.Helper()
	status, raw, _ := h.do(t, method, path, testAPIToken, body)
	if status != want {
		t.Fatalf("%s %s 状态码 = %d，期望 %d，响应 %s", method, path, status, want, raw)
	}
	return raw
}

// b5mcpDoTenant 向「绑定另一个租户」的服务发送请求。
func b5mcpDoTenant(t *testing.T, srv *httptest.Server, method, path, body string, want int) []byte {
	t.Helper()
	status, raw, _ := doAt(t, srv.Client(), srv.URL, method, path, testAPIToken, body)
	if status != want {
		t.Fatalf("[租户B] %s %s 状态码 = %d，期望 %d，响应 %s", method, path, status, want, raw)
	}
	return raw
}

// b5mcpTenantServer 为指定租户构建一个 HTTP 服务（共享同一个 Store）。
func b5mcpTenantServer(t *testing.T, h *insightsHarness, tenantID int64) *httptest.Server {
	t.Helper()
	opts := Options{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		RequestTimeout: 5 * time.Second,
		TenantResolver: StaticTenant(tenant.ID(tenantID)),
		Authenticator:  NewTokenAuthenticator(testAPIToken),
		TenantID:       tenantID,
		Store:          h.store,
	}
	handler, err := NewHandler(opts)
	if err != nil {
		t.Fatalf("构建租户 %d 的 HTTP 处理器失败: %v", tenantID, err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// b5mcpJSONBody 构造 JSON 请求体。
func b5mcpJSONBody(t *testing.T, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编码请求体失败: %v", err)
	}
	return string(raw)
}

// ---------- MCP mock 服务 ----------

// b5mcpMockMCP 是最小 MCP 服务：支持 initialize / notifications/initialized /
// tools/list，可切换 JSON 与 SSE 两种响应形态，也可切换为 500 失败。
type b5mcpMockMCP struct {
	mu      sync.Mutex
	tools   []map[string]any
	fail    bool
	sse     bool
	gotAuth string
}

func (m *b5mcpMockMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gotAuth = r.Header.Get("Authorization")
	if m.fail {
		http.Error(w, "mock 服务故意失败", http.StatusInternalServerError)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		ID     *int   `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	switch req.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "initialize":
		m.writeResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "b5-mock"},
		})
	case "tools/list":
		m.writeResult(w, req.ID, map[string]any{"tools": m.tools})
	default:
		m.writeError(w, req.ID, -32601, "method not found")
	}
}

func (m *b5mcpMockMCP) writeResult(w http.ResponseWriter, id *int, result any) {
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	raw, _ := json.Marshal(payload)
	if m.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (m *b5mcpMockMCP) writeError(w http.ResponseWriter, id *int, code int, message string) {
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
	raw, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// b5mcpNewMock 启动一个返回固定工具清单的 mock MCP。
func b5mcpNewMock(t *testing.T, tools []map[string]any) (*b5mcpMockMCP, *httptest.Server) {
	t.Helper()
	mock := &b5mcpMockMCP{tools: tools}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	return mock, srv
}

// b5mcpDefaultTools 是 mock MCP 返回的两条工具。
func b5mcpDefaultTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "search",
			"description": "搜索工具",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
			},
		},
		{
			"name":        "fetch",
			"description": "抓取工具",
			"inputSchema": map[string]any{"type": "object"},
		},
	}
}

// ---------- MCP 测试 ----------

// TestB5MCPCRUD 覆盖 P-B5-01..05、P-B5-07 的基础 CRUD 与掩码。
func TestB5MCPCRUD(t *testing.T) {
	h := newInsightsHarness(t)

	// 空列表必须是 [] 而不是 null。
	raw := h.b5mcpDo(t, http.MethodGet, "/api/v1/mcp/servers", "", http.StatusOK)
	var list struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析端点列表失败: %v", err)
	}
	if list.Servers == nil || len(list.Servers) != 0 {
		t.Fatalf("初始端点列表 = %v，期望空数组", list.Servers)
	}

	// 非法 transport / 非法 url / 空名称。
	h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers",
		b5mcpJSONBody(t, map[string]any{"name": "bad", "transport": "sse", "url": "https://example.com"}),
		http.StatusBadRequest)
	h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers",
		b5mcpJSONBody(t, map[string]any{"name": "bad2", "transport": "http", "url": "ftp://example.com"}),
		http.StatusBadRequest)
	h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers",
		b5mcpJSONBody(t, map[string]any{"name": "  ", "transport": "http", "url": "https://example.com"}),
		http.StatusBadRequest)

	// 创建（含敏感 env/headers）。
	created := h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers", b5mcpJSONBody(t, map[string]any{
		"name":      "alpha",
		"transport": "http",
		"url":       "https://example.com/mcp",
		"env":       map[string]string{"TOKEN": "super-secret-token", "PLAIN": "visible"},
		"headers":   map[string]string{"Authorization": "Bearer super-secret-token"},
	}), http.StatusCreated)
	var server map[string]any
	if err := json.Unmarshal(created, &server); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	env := server["env"].(map[string]any)
	if env["TOKEN"] != "***" {
		t.Fatalf("env.TOKEN 未掩码: %v", env["TOKEN"])
	}
	if env["PLAIN"] != "visible" {
		t.Fatalf("非敏感 env.PLAIN 被误掩码: %v", env["PLAIN"])
	}
	headers := server["headers"].(map[string]any)
	if headers["Authorization"] != "***" {
		t.Fatalf("headers.Authorization 未掩码: %v", headers["Authorization"])
	}
	if server["last_status"] != "unknown" {
		t.Fatalf("新端点 last_status = %v，期望 unknown", server["last_status"])
	}
	serverID := int64(server["id"].(float64))

	// 重名 → 409。
	h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers",
		b5mcpJSONBody(t, map[string]any{"name": "alpha", "transport": "http", "url": "https://example.com/other"}),
		http.StatusConflict)

	// 详情 / 局部更新 / 非法 id。
	h.b5mcpDo(t, http.MethodGet, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID), "", http.StatusOK)
	h.b5mcpDo(t, http.MethodGet, "/api/v1/mcp/servers/abc", "", http.StatusBadRequest)
	h.b5mcpDo(t, http.MethodGet, "/api/v1/mcp/servers/999999", "", http.StatusNotFound)

	updated := h.b5mcpDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID),
		b5mcpJSONBody(t, map[string]any{"name": "alpha-renamed", "enabled": false}), http.StatusOK)
	var updatedServer map[string]any
	if err := json.Unmarshal(updated, &updatedServer); err != nil {
		t.Fatalf("解析更新结果失败: %v", err)
	}
	if updatedServer["name"] != "alpha-renamed" || updatedServer["enabled"] != false {
		t.Fatalf("更新未生效: %v", updatedServer)
	}
	// env 未在 PATCH 中提供 → 保持原值（仍掩码）。
	if updatedServer["env"].(map[string]any)["TOKEN"] != "***" {
		t.Fatalf("未修改的 env 丢失: %v", updatedServer["env"])
	}
	// PATCH 把 url 清空 → 400（http 必须有 url）。
	h.b5mcpDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID),
		b5mcpJSONBody(t, map[string]any{"url": ""}), http.StatusBadRequest)

	// 工具列表：server_id 过滤、非法参数、空数组。
	raw = h.b5mcpDo(t, http.MethodGet, fmt.Sprintf("/api/v1/mcp/tools?server_id=%d", serverID), "", http.StatusOK)
	var tools struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("解析工具列表失败: %v", err)
	}
	if tools.Tools == nil || len(tools.Tools) != 0 {
		t.Fatalf("未刷新前的工具列表 = %v，期望空数组", tools.Tools)
	}
	h.b5mcpDo(t, http.MethodGet, "/api/v1/mcp/tools?server_id=bad", "", http.StatusBadRequest)

	// 删除 → 204，再取 → 404。
	h.b5mcpDo(t, http.MethodDelete, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID), "", http.StatusNoContent)
	h.b5mcpDo(t, http.MethodGet, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID), "", http.StatusNotFound)
	h.b5mcpDo(t, http.MethodDelete, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID), "", http.StatusNotFound)
}

// TestB5MCPRefreshHTTP 覆盖 P-B5-06 的成功/失败路径与敏感信息不落 last_error。
func TestB5MCPRefreshHTTP(t *testing.T) {
	h := newInsightsHarness(t)
	mock, mcpSrv := b5mcpNewMock(t, b5mcpDefaultTools())

	created := h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers", b5mcpJSONBody(t, map[string]any{
		"name":      "refresh-http",
		"transport": "http",
		"url":       mcpSrv.URL,
		"headers":   map[string]string{"Authorization": "Bearer dont-leak-me"},
	}), http.StatusCreated)
	var server map[string]any
	if err := json.Unmarshal(created, &server); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	serverID := int64(server["id"].(float64))

	// 刷新成功：返回 2 条工具，DB 中可查，状态 ok。
	raw := h.b5mcpDo(t, http.MethodPost, fmt.Sprintf("/api/v1/mcp/servers/%d/refresh", serverID), "", http.StatusOK)
	var refreshed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &refreshed); err != nil {
		t.Fatalf("解析刷新结果失败: %v", err)
	}
	if len(refreshed.Tools) != 2 {
		t.Fatalf("刷新得到 %d 条工具，期望 2", len(refreshed.Tools))
	}
	// 自定义 header 确实发出去了。
	mock.mu.Lock()
	gotAuth := mock.gotAuth
	mock.mu.Unlock()
	if gotAuth != "Bearer dont-leak-me" {
		t.Fatalf("mock 收到的 Authorization = %q", gotAuth)
	}

	raw = h.b5mcpDo(t, http.MethodGet, fmt.Sprintf("/api/v1/mcp/tools?server_id=%d", serverID), "", http.StatusOK)
	var listed struct {
		Tools []struct {
			Name        string         `json:"name"`
			ServerID    int64          `json:"server_id"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("解析工具列表失败: %v", err)
	}
	if len(listed.Tools) != 2 {
		t.Fatalf("入库工具数 = %d，期望 2", len(listed.Tools))
	}
	names := []string{listed.Tools[0].Name, listed.Tools[1].Name}
	sort.Strings(names)
	if strings.Join(names, ",") != "fetch,search" {
		t.Fatalf("工具名 = %v", names)
	}
	if listed.Tools[0].ServerID != serverID {
		t.Fatalf("工具 server_id = %d，期望 %d", listed.Tools[0].ServerID, serverID)
	}

	detail := h.b5mcpDo(t, http.MethodGet, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID), "", http.StatusOK)
	var detailServer map[string]any
	if err := json.Unmarshal(detail, &detailServer); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if detailServer["last_status"] != "ok" || detailServer["last_error"] != "" {
		t.Fatalf("刷新后状态 = %v / %v", detailServer["last_status"], detailServer["last_error"])
	}
	if detailServer["last_refreshed_at"] == nil {
		t.Fatal("刷新后 last_refreshed_at 为空")
	}

	// 刷新失败：502 + last_status=error + last_error 脱敏；上次成功的工具清单保留。
	mock.mu.Lock()
	mock.fail = true
	mock.mu.Unlock()
	h.b5mcpDo(t, http.MethodPost, fmt.Sprintf("/api/v1/mcp/servers/%d/refresh", serverID), "", http.StatusBadGateway)
	detail = h.b5mcpDo(t, http.MethodGet, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID), "", http.StatusOK)
	if err := json.Unmarshal(detail, &detailServer); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if detailServer["last_status"] != "error" {
		t.Fatalf("失败后 last_status = %v，期望 error", detailServer["last_status"])
	}
	lastError, _ := detailServer["last_error"].(string)
	if strings.TrimSpace(lastError) == "" {
		t.Fatal("失败后 last_error 为空")
	}
	if strings.Contains(lastError, "dont-leak-me") {
		t.Fatalf("last_error 泄漏了请求头凭据: %s", lastError)
	}
	raw = h.b5mcpDo(t, http.MethodGet, fmt.Sprintf("/api/v1/mcp/tools?server_id=%d", serverID), "", http.StatusOK)
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("解析工具列表失败: %v", err)
	}
	if len(listed.Tools) != 2 {
		t.Fatalf("失败刷新不应清空旧清单，当前 %d 条", len(listed.Tools))
	}

	// 刷新不存在的端点 → 404。
	h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers/999999/refresh", "", http.StatusNotFound)
}

// TestB5MCPRefreshSSE 覆盖 SSE（text/event-stream）响应形态。
func TestB5MCPRefreshSSE(t *testing.T) {
	h := newInsightsHarness(t)
	mock, mcpSrv := b5mcpNewMock(t, b5mcpDefaultTools())
	mock.sse = true

	created := h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers", b5mcpJSONBody(t, map[string]any{
		"name": "refresh-sse", "transport": "http", "url": mcpSrv.URL,
	}), http.StatusCreated)
	var server map[string]any
	if err := json.Unmarshal(created, &server); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	serverID := int64(server["id"].(float64))
	raw := h.b5mcpDo(t, http.MethodPost, fmt.Sprintf("/api/v1/mcp/servers/%d/refresh", serverID), "", http.StatusOK)
	var refreshed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &refreshed); err != nil {
		t.Fatalf("解析刷新结果失败: %v", err)
	}
	if len(refreshed.Tools) != 2 {
		t.Fatalf("SSE 刷新得到 %d 条工具，期望 2", len(refreshed.Tools))
	}
}

// TestB5MCPStdioPolicy 覆盖 stdio 默认禁用、开启后命令失败/白名单两条路径。
func TestB5MCPStdioPolicy(t *testing.T) {
	h := newInsightsHarness(t)

	// 默认禁用：创建 stdio 端点 → 400。
	h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers", b5mcpJSONBody(t, map[string]any{
		"name": "stdio-off", "transport": "stdio", "command": "/bin/echo",
	}), http.StatusBadRequest)

	// 开启后允许创建；刷新时命令不存在 → 502（只验证安全路径，不执行真实命令）。
	t.Setenv("BEACONX_MCP_ALLOW_STDIO", "true")
	created := h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers", b5mcpJSONBody(t, map[string]any{
		"name": "stdio-on", "transport": "stdio", "command": "beaconx-b5-nonexistent-command",
	}), http.StatusCreated)
	var server map[string]any
	if err := json.Unmarshal(created, &server); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	serverID := int64(server["id"].(float64))
	h.b5mcpDo(t, http.MethodPost, fmt.Sprintf("/api/v1/mcp/servers/%d/refresh", serverID), "", http.StatusBadGateway)

	// 白名单不匹配：命令不在允许列表 → 刷新 502，且错误里点明不在白名单。
	t.Setenv("BEACONX_MCP_STDIO_ALLOWLIST", "/usr/local/bin/other-mcp")
	raw := h.doRaw(t, http.MethodPost, fmt.Sprintf("/api/v1/mcp/servers/%d/refresh", serverID), "")
	if !bytes.Contains(raw, []byte("白名单")) && !bytes.Contains(raw, []byte("允许列表")) {
		t.Fatalf("白名单拒绝的错误信息不明确: %s", raw)
	}
}

// doRaw 返回状态码与响应体，不做状态码断言（用于断言错误文本）。
func (h *insightsHarness) doRaw(t *testing.T, method, path, body string) []byte {
	t.Helper()
	_, raw, _ := h.do(t, method, path, testAPIToken, body)
	return raw
}

// TestB5MCPTenantIsolation 校验端点与工具严格按租户隔离。
func TestB5MCPTenantIsolation(t *testing.T) {
	h := newInsightsHarness(t)
	tenantB := b5mcpTenantServer(t, h, h.tenantB)

	created := h.b5mcpDo(t, http.MethodPost, "/api/v1/mcp/servers", b5mcpJSONBody(t, map[string]any{
		"name": "tenant-a-only", "transport": "http", "url": "https://example.com/mcp",
	}), http.StatusCreated)
	var server map[string]any
	if err := json.Unmarshal(created, &server); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	serverID := int64(server["id"].(float64))

	raw := b5mcpDoTenant(t, tenantB, http.MethodGet, "/api/v1/mcp/servers", "", http.StatusOK)
	var list struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析租户B列表失败: %v", err)
	}
	if len(list.Servers) != 0 {
		t.Fatalf("租户B看到了租户A的端点: %v", list.Servers)
	}
	b5mcpDoTenant(t, tenantB, http.MethodGet, fmt.Sprintf("/api/v1/mcp/servers/%d", serverID), "", http.StatusNotFound)
	b5mcpDoTenant(t, tenantB, http.MethodPost, fmt.Sprintf("/api/v1/mcp/servers/%d/refresh", serverID), "", http.StatusNotFound)
}

// TestB5MCPStoreUnavailable 校验 Store 缺失时所有 MCP 接口返回 503。
func TestB5MCPStoreUnavailable(t *testing.T) {
	h := newInsightsHarness(t)
	srv := h.newServer(t, false, false)
	for _, rt := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/mcp/servers"},
		{http.MethodPost, "/api/v1/mcp/servers"},
		{http.MethodGet, "/api/v1/mcp/servers/1"},
		{http.MethodPatch, "/api/v1/mcp/servers/1"},
		{http.MethodDelete, "/api/v1/mcp/servers/1"},
		{http.MethodPost, "/api/v1/mcp/servers/1/refresh"},
		{http.MethodGet, "/api/v1/mcp/tools"},
	} {
		status, raw, _ := doAt(t, srv.Client(), srv.URL, rt.method, rt.path, testAPIToken, "{}")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("%s %s 状态码 = %d，期望 503，响应 %s", rt.method, rt.path, status, raw)
		}
	}
}

// ---------- Skills 测试 ----------

// b5mcpUseSkillsDir 把技能根目录指向临时目录。
func b5mcpUseSkillsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BEACONX_SKILLS_DIR", dir)
	return dir
}

// b5mcpZip 构造 zip；entries 按名字排序保证可复现。
func b5mcpZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	entries := make([]b5mcpZipEntry, 0, len(files))
	for name, body := range files {
		entries = append(entries, b5mcpZipEntry{Name: name, Body: body})
	}
	return b5mcpZipEntries(t, entries)
}

// b5mcpZipEntry 是构造 zip 的一个条目。
type b5mcpZipEntry struct {
	Name string
	Body string
	Mode os.FileMode
}

// b5mcpZipEntries 按给定条目构造 zip。
func b5mcpZipEntries(t *testing.T, entries []b5mcpZipEntry) []byte {
	t.Helper()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.Name, Method: zip.Deflate}
		if entry.Mode != 0 {
			header.SetMode(entry.Mode)
		}
		w, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("创建 zip 条目 %q 失败: %v", entry.Name, err)
		}
		if _, err := w.Write([]byte(entry.Body)); err != nil {
			t.Fatalf("写入 zip 条目 %q 失败: %v", entry.Name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	return buf.Bytes()
}

// b5mcpUpload 以 multipart 上传 zip。
func b5mcpUpload(t *testing.T, client *http.Client, baseURL, path string, data []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", "skill.zip")
	if err != nil {
		t.Fatalf("创建 multipart 字段失败: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("写入 multipart 失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, &buf)
	if err != nil {
		t.Fatalf("构造上传请求失败: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("上传请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取上传响应失败: %v", err)
	}
	return resp.StatusCode, raw
}

// TestB5SkillsCRUD 覆盖 P-B5-08..12、P-B5-14..17 的基础路径。
func TestB5SkillsCRUD(t *testing.T) {
	h := newInsightsHarness(t)
	root := b5mcpUseSkillsDir(t)

	// 空列表必须是 []。
	raw := h.b5mcpDo(t, http.MethodGet, "/api/v1/skills", "", http.StatusOK)
	var list struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析技能列表失败: %v", err)
	}
	if list.Skills == nil || len(list.Skills) != 0 {
		t.Fatalf("初始技能列表 = %v，期望空数组", list.Skills)
	}

	// 非法名 → 400。
	h.b5mcpDo(t, http.MethodPost, "/api/v1/skills", b5mcpJSONBody(t, map[string]any{"name": "Bad Name"}), http.StatusBadRequest)
	h.b5mcpDo(t, http.MethodPost, "/api/v1/skills", b5mcpJSONBody(t, map[string]any{"name": "../escape"}), http.StatusBadRequest)

	// 创建 → 201，目录里生成 SKILL.md。
	created := h.b5mcpDo(t, http.MethodPost, "/api/v1/skills",
		b5mcpJSONBody(t, map[string]any{"name": "alpha", "description": "第一个技能"}), http.StatusCreated)
	var skill map[string]any
	if err := json.Unmarshal(created, &skill); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	if skill["name"] != "alpha" || skill["files"].(float64) != 1 {
		t.Fatalf("创建结果不符合预期: %v", skill)
	}
	skillMD := filepath.Join(root, fmt.Sprintf("%d", h.tenantA), "alpha", "SKILL.md")
	if _, err := os.Stat(skillMD); err != nil {
		t.Fatalf("SKILL.md 未生成: %v", err)
	}
	// 重名 → 409；非法/不存在 → 400/404。
	h.b5mcpDo(t, http.MethodPost, "/api/v1/skills", b5mcpJSONBody(t, map[string]any{"name": "alpha"}), http.StatusConflict)
	h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/BadName", "", http.StatusBadRequest)
	h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/missing", "", http.StatusNotFound)

	// 更新元数据。
	updated := h.b5mcpDo(t, http.MethodPatch, "/api/v1/skills/alpha",
		b5mcpJSONBody(t, map[string]any{"description": "改过的描述", "version": "1.2.3"}), http.StatusOK)
	var updatedSkill map[string]any
	if err := json.Unmarshal(updated, &updatedSkill); err != nil {
		t.Fatalf("解析更新结果失败: %v", err)
	}
	if updatedSkill["description"] != "改过的描述" || updatedSkill["version"] != "1.2.3" {
		t.Fatalf("元数据更新未生效: %v", updatedSkill)
	}

	// 文件列表 + 写入 + 读取 + 删除。
	raw = h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/alpha/files", "", http.StatusOK)
	var files struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatalf("解析文件列表失败: %v", err)
	}
	if len(files.Files) != 1 || files.Files[0]["path"] != "SKILL.md" {
		t.Fatalf("文件列表 = %v", files.Files)
	}

	h.b5mcpDo(t, http.MethodPut, "/api/v1/skills/alpha/files/docs/notes.md",
		b5mcpJSONBody(t, map[string]any{"content": "# 笔记\n"}), http.StatusOK)
	raw = h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/alpha/files/docs/notes.md", "", http.StatusOK)
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("解析读取结果失败: %v", err)
	}
	if file["content"] != "# 笔记\n" {
		t.Fatalf("读取内容 = %v", file["content"])
	}
	// 写入后元数据文件数已同步为 2。
	detail := h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/alpha", "", http.StatusOK)
	var detailSkill map[string]any
	if err := json.Unmarshal(detail, &detailSkill); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if detailSkill["files"].(float64) != 2 || detailSkill["file_count"].(float64) != 2 {
		t.Fatalf("文件数未同步: %v", detailSkill)
	}
	// 非文本扩展名 → 400。
	h.b5mcpDo(t, http.MethodPut, "/api/v1/skills/alpha/files/run.exe",
		b5mcpJSONBody(t, map[string]any{"content": "x"}), http.StatusBadRequest)
	// 读不存在的文件 → 404；读目录 → 400。
	h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/alpha/files/none.md", "", http.StatusNotFound)
	h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/alpha/files/docs", "", http.StatusBadRequest)

	h.b5mcpDo(t, http.MethodDelete, "/api/v1/skills/alpha/files/docs/notes.md", "", http.StatusNoContent)
	h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/alpha/files/docs/notes.md", "", http.StatusNotFound)

	// 删除技能 → 204，目录消失，再取 404。
	h.b5mcpDo(t, http.MethodDelete, "/api/v1/skills/alpha", "", http.StatusNoContent)
	if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("%d", h.tenantA), "alpha")); !os.IsNotExist(err) {
		t.Fatalf("删除技能后目录仍存在: %v", err)
	}
	h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/alpha", "", http.StatusNotFound)
}

// TestB5SkillsPathSafety 覆盖路径穿越/反斜杠/越界读取写入。
func TestB5SkillsPathSafety(t *testing.T) {
	h := newInsightsHarness(t)
	root := b5mcpUseSkillsDir(t)
	h.b5mcpDo(t, http.MethodPost, "/api/v1/skills", b5mcpJSONBody(t, map[string]any{"name": "safe"}), http.StatusCreated)

	// 反斜杠与 NUL 由业务层拒绝（400）；".." 可能被 ServeMux 先归一化，接受 400/404。
	for _, path := range []string{
		"/api/v1/skills/safe/files/dir%5Cfile.md",
		"/api/v1/skills/safe/files/..%2Fescape.md",
		"/api/v1/skills/safe/files/%2e%2e%2Fescape.md",
	} {
		status, raw, _ := h.do(t, http.MethodPut, path, testAPIToken,
			b5mcpJSONBody(t, map[string]any{"content": "x"}))
		if status != http.StatusBadRequest && status != http.StatusNotFound && status != http.StatusMovedPermanently {
			t.Fatalf("PUT %s 状态码 = %d（期望 400/404/301），响应 %s", path, status, raw)
		}
	}
	// 确认技能目录之外没有落盘文件。
	tenantDir := filepath.Join(root, fmt.Sprintf("%d", h.tenantA))
	var escaped []string
	_ = filepath.WalkDir(tenantDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(path) == "escape.md" {
			escaped = append(escaped, path)
		}
		return nil
	})
	if len(escaped) > 0 {
		t.Fatalf("路径穿越写入了文件: %v", escaped)
	}
}

// TestB5SkillsUpload 覆盖 P-B5-13 的合法 zip、重名、覆盖上传。
func TestB5SkillsUpload(t *testing.T) {
	h := newInsightsHarness(t)
	b5mcpUseSkillsDir(t)

	zipData := b5mcpZip(t, map[string]string{
		"SKILL.md":        "---\nname: zeta\ndescription: 上传的技能\n---\n\n# zeta\n",
		"scripts/run.py":  "print('hi')\n",
		"references/a.md": "# 参考\n",
	})
	status, raw := b5mcpUpload(t, h.client, h.baseURL, "/api/v1/skills/upload", zipData)
	if status != http.StatusCreated {
		t.Fatalf("上传状态码 = %d，响应 %s", status, raw)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析上传结果失败: %v", err)
	}
	if result["name"] != "zeta" || result["files"].(float64) != 3 {
		t.Fatalf("上传结果 = %v", result)
	}
	// 文件树可读到上传的文件。
	raw = h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/zeta/files", "", http.StatusOK)
	var files struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatalf("解析文件列表失败: %v", err)
	}
	if len(files.Files) != 3 {
		t.Fatalf("上传后文件数 = %d，期望 3", len(files.Files))
	}
	// 详情里的 description 来自 front matter。
	detail := h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/zeta", "", http.StatusOK)
	var skill map[string]any
	if err := json.Unmarshal(detail, &skill); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if skill["description"] != "上传的技能" {
		t.Fatalf("description = %v", skill["description"])
	}

	// 重名 → 409；?overwrite=true → 201 并替换内容。
	status, _ = b5mcpUpload(t, h.client, h.baseURL, "/api/v1/skills/upload", zipData)
	if status != http.StatusConflict {
		t.Fatalf("重复上传状态码 = %d，期望 409", status)
	}
	replacement := b5mcpZip(t, map[string]string{
		"SKILL.md": "---\nname: zeta\ndescription: 覆盖后的技能\n---\n",
	})
	status, raw = b5mcpUpload(t, h.client, h.baseURL, "/api/v1/skills/upload?overwrite=true", replacement)
	if status != http.StatusCreated {
		t.Fatalf("覆盖上传状态码 = %d，响应 %s", status, raw)
	}
	detail = h.b5mcpDo(t, http.MethodGet, "/api/v1/skills/zeta", "", http.StatusOK)
	if err := json.Unmarshal(detail, &skill); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if skill["description"] != "覆盖后的技能" || skill["files"].(float64) != 1 {
		t.Fatalf("覆盖后技能 = %v", skill)
	}
}

// TestB5SkillsUploadRejects 覆盖 zip 校验的拒绝路径。
func TestB5SkillsUploadRejects(t *testing.T) {
	h := newInsightsHarness(t)
	b5mcpUseSkillsDir(t)

	cases := []struct {
		name string
		data []byte
	}{
		{
			name: "路径穿越",
			data: b5mcpZip(t, map[string]string{
				"SKILL.md":     "---\nname: evil1\n---\n",
				"../escape.md": "x",
			}),
		},
		{
			name: "绝对路径",
			data: b5mcpZip(t, map[string]string{
				"SKILL.md":    "---\nname: evil2\n---\n",
				"/etc/passwd": "x",
			}),
		},
		{
			name: "符号链接",
			data: b5mcpZipEntries(t, []b5mcpZipEntry{
				{Name: "SKILL.md", Body: "---\nname: evil3\n---\n"},
				{Name: "link.md", Body: "../../etc/passwd", Mode: os.ModeSymlink | 0o777},
			}),
		},
		{
			name: "非法扩展名",
			data: b5mcpZip(t, map[string]string{
				"SKILL.md": "---\nname: evil4\n---\n",
				"bin.exe":  "x",
			}),
		},
		{
			name: "缺少 SKILL.md",
			data: b5mcpZip(t, map[string]string{"readme.md": "# 没有技能定义\n"}),
		},
		{
			name: "姓名非法回退失败",
			data: b5mcpZip(t, map[string]string{
				"SKILL.md": "---\nname: Bad Name\n---\n",
			}),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, raw := b5mcpUpload(t, h.client, h.baseURL, "/api/v1/skills/upload", testCase.data)
			if status != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400，响应 %s", status, raw)
			}
		})
	}

	// 文件数超限（201 个文件）。
	manyFiles := map[string]string{"SKILL.md": "---\nname: toomany\n---\n"}
	for i := 0; i < 201; i++ {
		manyFiles[fmt.Sprintf("f%03d.md", i)] = "x"
	}
	status, raw := b5mcpUpload(t, h.client, h.baseURL, "/api/v1/skills/upload", b5mcpZip(t, manyFiles))
	if status != http.StatusBadRequest {
		t.Fatalf("文件数超限状态码 = %d，期望 400，响应 %s", status, raw)
	}

	// 解压体积超限（60 × 250KB ≈ 15MiB，压缩后仍小于 10MiB）。
	big := map[string]string{"SKILL.md": "---\nname: toobig\n---\n"}
	chunk := strings.Repeat("A", 250<<10)
	for i := 0; i < 60; i++ {
		big[fmt.Sprintf("big%02d.txt", i)] = chunk
	}
	status, raw = b5mcpUpload(t, h.client, h.baseURL, "/api/v1/skills/upload", b5mcpZip(t, big))
	if status != http.StatusBadRequest {
		t.Fatalf("体积超限状态码 = %d，期望 400，响应 %s", status, raw)
	}
}

// TestB5SkillsTenantIsolation 校验同名技能在不同租户下互不可见。
func TestB5SkillsTenantIsolation(t *testing.T) {
	h := newInsightsHarness(t)
	root := b5mcpUseSkillsDir(t)
	tenantB := b5mcpTenantServer(t, h, h.tenantB)

	h.b5mcpDo(t, http.MethodPost, "/api/v1/skills", b5mcpJSONBody(t, map[string]any{"name": "shared-name"}), http.StatusCreated)
	h.b5mcpDo(t, http.MethodPut, "/api/v1/skills/shared-name/files/a.md",
		b5mcpJSONBody(t, map[string]any{"content": "A 租户内容"}), http.StatusOK)

	// 租户 B 看不到 A 的技能，也读不到 A 的文件。
	raw := b5mcpDoTenant(t, tenantB, http.MethodGet, "/api/v1/skills", "", http.StatusOK)
	var list struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析租户B技能列表失败: %v", err)
	}
	if len(list.Skills) != 0 {
		t.Fatalf("租户B看到了租户A的技能: %v", list.Skills)
	}
	b5mcpDoTenant(t, tenantB, http.MethodGet, "/api/v1/skills/shared-name", "", http.StatusNotFound)
	b5mcpDoTenant(t, tenantB, http.MethodGet, "/api/v1/skills/shared-name/files/a.md", "", http.StatusNotFound)

	// 租户 B 可以创建同名技能，磁盘目录互相隔离。
	b5mcpDoTenant(t, tenantB, http.MethodPost, "/api/v1/skills", b5mcpJSONBody(t, map[string]any{"name": "shared-name"}), http.StatusCreated)
	dirA := filepath.Join(root, fmt.Sprintf("%d", h.tenantA), "shared-name")
	dirB := filepath.Join(root, fmt.Sprintf("%d", h.tenantB), "shared-name")
	if _, err := os.Stat(dirA); err != nil {
		t.Fatalf("租户A目录不存在: %v", err)
	}
	if _, err := os.Stat(dirB); err != nil {
		t.Fatalf("租户B目录不存在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirB, "a.md")); !os.IsNotExist(err) {
		t.Fatalf("租户B目录出现了租户A的文件")
	}
}

// TestB5SkillsStoreUnavailable 校验 Store 缺失时技能接口返回 503。
func TestB5SkillsStoreUnavailable(t *testing.T) {
	h := newInsightsHarness(t)
	b5mcpUseSkillsDir(t)
	srv := h.newServer(t, false, false)
	status, raw, _ := doAt(t, srv.Client(), srv.URL, http.MethodGet, "/api/v1/skills", testAPIToken, "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET /skills 状态码 = %d，期望 503，响应 %s", status, raw)
	}
}

// TestB5SkillsMissingStat 校验「元数据文件数 > 磁盘实际文件数」时 missing 计数。
func TestB5SkillsMissingStat(t *testing.T) {
	h := newInsightsHarness(t)
	root := b5mcpUseSkillsDir(t)
	h.b5mcpDo(t, http.MethodPost, "/api/v1/skills", b5mcpJSONBody(t, map[string]any{"name": "gone"}), http.StatusCreated)
	// 绕过 API 直接删掉磁盘文件，模拟外部删除。
	if err := os.Remove(filepath.Join(root, fmt.Sprintf("%d", h.tenantA), "gone", "SKILL.md")); err != nil {
		t.Fatalf("删除磁盘文件失败: %v", err)
	}
	raw := h.b5mcpDo(t, http.MethodGet, "/api/v1/skills", "", http.StatusOK)
	var list struct {
		Skills []struct {
			Name    string `json:"name"`
			Files   int    `json:"files"`
			Missing int    `json:"missing"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析技能列表失败: %v", err)
	}
	if len(list.Skills) != 1 || list.Skills[0].Files != 0 || list.Skills[0].Missing != 1 {
		t.Fatalf("缺失统计不符合预期: %+v", list.Skills)
	}
}
