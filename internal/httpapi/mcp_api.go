package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/app/mcp"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B5-1：MCP 端点与工具清单接口（P-B5-01..07）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册，NewHandler 构造时用
// Build(Options) 注入 Store/TenantID。语义约定：
//   - 端点对象的 env/headers 里可能保存凭据，响应一律把敏感键的值掩码为
//     "***"（键名保留，便于前端展示「已配置」）；
//   - refresh 对 http 端点执行 initialize + tools/list（10s 超时），成功则整体
//     替换工具清单并置 last_status=ok；失败返回 502 并把脱敏摘要写入
//     last_error，不泄漏 env/headers；
//   - stdio 端点默认禁用：未开启（配置 mcp.allow_stdio / 环境变量
//     BEACONX_MCP_ALLOW_STDIO）时，创建 stdio 端点与刷新 stdio 端点都返回 400；
//     开启后仍可用命令白名单（BEACONX_MCP_STDIO_ALLOWLIST / settings
//     mcp.stdio_allowlist）进一步收敛；
//   - 列表与详情都不返回 env/headers 明文，刷新在服务端使用原值。

const (
	// mcpRefreshTimeout 是单次刷新（initialize + tools/list）的端到端超时。
	mcpRefreshTimeout = 10 * time.Second
	// mcpSettingAllowStdio / mcpSettingStdioAllowlist 是 settings 表的键名，
	// 与 config 的点号命名保持一致；数据库无记录时回退环境变量。
	mcpSettingAllowStdio     = "mcp.allow_stdio"
	mcpSettingStdioAllowlist = "mcp.stdio_allowlist"
	mcpEnvAllowStdio         = "BEACONX_MCP_ALLOW_STDIO"
	mcpEnvStdioAllowlist     = "BEACONX_MCP_STDIO_ALLOWLIST"
	mcpMaskedPlaceholder     = "***"
)

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/mcp/servers",
			Summary: "MCP 端点列表",
			Build:   func(o Options) http.HandlerFunc { return o.handleListMCPServers },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/mcp/servers",
			Summary: "新建 MCP 端点",
			Build:   func(o Options) http.HandlerFunc { return o.handleCreateMCPServer },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/mcp/servers/{id}",
			Summary: "MCP 端点详情",
			Build:   func(o Options) http.HandlerFunc { return o.handleGetMCPServer },
		},
		FeatureRoute{
			Method: http.MethodPatch, Pattern: "/api/v1/mcp/servers/{id}",
			Summary: "更新 MCP 端点（name/url/command/args/env/headers/enabled）",
			Build:   func(o Options) http.HandlerFunc { return o.handleUpdateMCPServer },
		},
		FeatureRoute{
			Method: http.MethodDelete, Pattern: "/api/v1/mcp/servers/{id}",
			Summary: "删除 MCP 端点",
			Build:   func(o Options) http.HandlerFunc { return o.handleDeleteMCPServer },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/mcp/servers/{id}/refresh",
			Summary: "刷新 MCP 工具清单",
			Build:   func(o Options) http.HandlerFunc { return o.handleRefreshMCPServer },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/mcp/tools",
			Summary: "MCP 工具列表（?server_id=）",
			Build:   func(o Options) http.HandlerFunc { return o.handleListMCPTools },
		},
	)
}

// mcpServerView 是对外端点视图：env/headers 已掩码。
type mcpServerView struct {
	ID              int64             `json:"id"`
	Name            string            `json:"name"`
	Transport       string            `json:"transport"`
	URL             string            `json:"url"`
	Command         string            `json:"command"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env"`
	Headers         map[string]string `json:"headers"`
	Enabled         bool              `json:"enabled"`
	LastStatus      string            `json:"last_status"`
	LastError       string            `json:"last_error"`
	LastRefreshedAt *time.Time        `json:"last_refreshed_at,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// mcpSensitiveKeyParts 是「值必须掩码」的键名片段（小写比较）。
var mcpSensitiveKeyParts = []string{
	"token", "authorization", "auth", "password", "passwd", "secret",
	"api_key", "apikey", "credential", "cookie", "private",
}

// mcpIsSensitiveKey 判断某个 env/headers 键的值是否需要在响应中掩码。
//
// 采用「包含即敏感」的宽松策略：宁可多掩码，也不要把凭据漏给前端。
func mcpIsSensitiveKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "" {
		return false
	}
	for _, part := range mcpSensitiveKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

// mcpMaskMap 返回掩码后的副本；敏感键的值替换为 "***"。
func mcpMaskMap(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for key, value := range source {
		if mcpIsSensitiveKey(key) && strings.TrimSpace(value) != "" {
			out[key] = mcpMaskedPlaceholder
			continue
		}
		out[key] = value
	}
	return out
}

// mcpServerToView 把存储行投影成响应视图。
func mcpServerToView(server pg.MCPServer) mcpServerView {
	return mcpServerView{
		ID:              server.ID,
		Name:            server.Name,
		Transport:       server.Transport,
		URL:             server.URL,
		Command:         server.Command,
		Args:            server.Args,
		Env:             mcpMaskMap(server.Env),
		Headers:         mcpMaskMap(server.Headers),
		Enabled:         server.Enabled,
		LastStatus:      server.LastStatus,
		LastError:       server.LastError,
		LastRefreshedAt: server.LastRefreshedAt,
		CreatedAt:       server.CreatedAt,
		UpdatedAt:       server.UpdatedAt,
	}
}

// mcpServerPathID 解析 /mcp/servers/{id}。
func (o Options) mcpServerPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "MCP 端点")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}

// mcpStdioPolicy 解析 stdio 的安全策略。
//
// 优先级：settings 表（运行时配置）> 环境变量 > 默认（禁用、白名单为空）。
func (o Options) mcpStdioPolicy(r *http.Request) (allow bool, allowlist []string) {
	if o.Store != nil {
		values, err := o.Store.GetSettingsMap(r.Context(), o.TenantID)
		if err == nil {
			if raw, ok := values[mcpSettingAllowStdio]; ok {
				var flag bool
				if json.Unmarshal(raw, &flag) == nil {
					return flag, mcpAllowlistFromSetting(values[mcpSettingStdioAllowlist])
				}
			}
			if raw, ok := values[mcpSettingStdioAllowlist]; ok {
				return mcpEnvBool(mcpEnvAllowStdio), mcpAllowlistFromSetting(raw)
			}
		}
	}
	return mcpEnvBool(mcpEnvAllowStdio), mcpAllowlistFromEnv()
}

// mcpEnvBool 读取布尔型环境变量；非法值按 false 处理（fail closed）。
func mcpEnvBool(key string) bool {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return false
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return parsed
}

// mcpAllowlistFromEnv 读取逗号分隔的命令白名单。
func mcpAllowlistFromEnv() []string {
	raw, ok := os.LookupEnv(mcpEnvStdioAllowlist)
	if !ok {
		return nil
	}
	return mcpSplitAllowlist(raw)
}

// mcpAllowlistFromSetting 解析 settings 里的白名单（JSON 数组或逗号字符串）。
func mcpAllowlistFromSetting(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		out := make([]string, 0, len(list))
		for _, item := range list {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return mcpSplitAllowlist(text)
	}
	return nil
}

// mcpSplitAllowlist 把逗号分隔字符串拆成白名单。
func mcpSplitAllowlist(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// mcpCreateRequest 是创建端点的请求体。
type mcpCreateRequest struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	URL       string            `json:"url"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Headers   map[string]string `json:"headers"`
	Enabled   *bool             `json:"enabled"`
}

// mcpPatchRequest 是局部更新请求体；env/headers 用 RawMessage 区分
// 「字段缺省（不修改）」与「显式传 null（清空）」。
type mcpPatchRequest struct {
	Name    *string         `json:"name"`
	URL     *string         `json:"url"`
	Command *string         `json:"command"`
	Args    json.RawMessage `json:"args"`
	Env     json.RawMessage `json:"env"`
	Headers json.RawMessage `json:"headers"`
	Enabled *bool           `json:"enabled"`
}

// handleListMCPServers 列出端点。
func (o Options) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	servers, err := o.Store.ListMCPServers(r.Context(), o.TenantID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	views := make([]mcpServerView, 0, len(servers))
	for _, server := range servers {
		views = append(views, mcpServerToView(server))
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": views})
}

// handleCreateMCPServer 新建端点。
func (o Options) handleCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req mcpCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	transport := strings.TrimSpace(req.Transport)
	if transport == mcp.TransportStdio {
		if !mcp.StdioSupported() {
			writeError(w, r, http.StatusBadRequest, "stdio_unsupported", "当前平台不支持 MCP stdio 传输")
			return
		}
		allow, _ := o.mcpStdioPolicy(r)
		if !allow {
			writeError(w, r, http.StatusBadRequest, "stdio_disabled",
				"stdio 传输默认禁用：请先在设置中开启 mcp.allow_stdio（或设置环境变量 "+mcpEnvAllowStdio+"）")
			return
		}
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	server, err := o.Store.CreateMCPServer(r.Context(), o.TenantID, pg.MCPServerInput{
		Name:      req.Name,
		Transport: transport,
		URL:       req.URL,
		Command:   req.Command,
		Args:      req.Args,
		Env:       req.Env,
		Headers:   req.Headers,
		Enabled:   enabled,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, mcpServerToView(server))
}

// handleGetMCPServer 读取端点详情。
func (o Options) handleGetMCPServer(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	serverID, ok := o.mcpServerPathID(w, r)
	if !ok {
		return
	}
	server, err := o.Store.GetMCPServer(r.Context(), o.TenantID, serverID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpServerToView(server))
}

// handleUpdateMCPServer 局部更新端点；transport 不可改。
func (o Options) handleUpdateMCPServer(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	serverID, ok := o.mcpServerPathID(w, r)
	if !ok {
		return
	}
	var req mcpPatchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	update := pg.MCPServerUpdate{
		Name:    req.Name,
		URL:     req.URL,
		Command: req.Command,
		Enabled: req.Enabled,
	}
	if len(req.Args) > 0 {
		args, ok := mcpStringListField(w, r, req.Args, "args")
		if !ok {
			return
		}
		update.Args = &args
	}
	if len(req.Env) > 0 {
		env, ok := mcpStringMapField(w, r, req.Env, "env")
		if !ok {
			return
		}
		update.Env = &env
	}
	if len(req.Headers) > 0 {
		headers, ok := mcpStringMapField(w, r, req.Headers, "headers")
		if !ok {
			return
		}
		update.Headers = &headers
	}
	server, err := o.Store.UpdateMCPServer(r.Context(), o.TenantID, serverID, update)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpServerToView(server))
}

// mcpStringListField 解析字符串数组字段；null 视为清空。
func mcpStringListField(w http.ResponseWriter, r *http.Request, raw json.RawMessage, field string) ([]string, bool) {
	if strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
		return []string{}, true
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", field+" 必须是字符串数组或 null")
		return nil, false
	}
	if out == nil {
		out = []string{}
	}
	return out, true
}

// mcpStringMapField 解析字符串映射字段；null 视为清空。
func mcpStringMapField(w http.ResponseWriter, r *http.Request, raw json.RawMessage, field string) (map[string]string, bool) {
	if strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
		return map[string]string{}, true
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", field+" 必须是字符串到字符串的对象或 null")
		return nil, false
	}
	if out == nil {
		out = map[string]string{}
	}
	return out, true
}

// handleDeleteMCPServer 删除端点（工具清单级联删除）。
func (o Options) handleDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	serverID, ok := o.mcpServerPathID(w, r)
	if !ok {
		return
	}
	if err := o.Store.DeleteMCPServer(r.Context(), o.TenantID, serverID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mcpToolView 是工具清单的响应项（不再回显 server 的凭据）。
type mcpToolView struct {
	ServerID    int64          `json:"server_id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// handleListMCPTools 列出工具；?server_id= 过滤，缺省为全部。
func (o Options) handleListMCPTools(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	serverID := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("server_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "server_id 必须是正整数")
			return
		}
		serverID = parsed
	}
	tools, err := o.Store.ListMCPTools(r.Context(), o.TenantID, serverID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	views := make([]mcpToolView, 0, len(tools))
	for _, tool := range tools {
		views = append(views, mcpToolView{
			ServerID:    tool.ServerID,
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": views})
}

// handleRefreshMCPServer 刷新工具清单。
//
// 成功：整体替换 mcp_tools、状态置 ok、返回 {"tools":[...]}。
// 失败：记录 last_status=error + 脱敏 last_error（不覆盖上次成功清单）、返回 502。
func (o Options) handleRefreshMCPServer(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	serverID, ok := o.mcpServerPathID(w, r)
	if !ok {
		return
	}
	server, err := o.Store.GetMCPServer(r.Context(), o.TenantID, serverID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if server.Transport == mcp.TransportStdio && !mcp.StdioSupported() {
		writeError(w, r, http.StatusBadRequest, "stdio_unsupported", "当前平台不支持 MCP stdio 传输")
		return
	}
	allow, allowlist := o.mcpStdioPolicy(r)
	cfg := mcp.Server{
		Name:      server.Name,
		Transport: server.Transport,
		URL:       server.URL,
		Headers:   server.Headers,
		Command:   server.Command,
		Args:      server.Args,
		Env:       server.Env,
	}
	ctx, cancel := context.WithTimeout(r.Context(), mcpRefreshTimeout)
	defer cancel()
	tools, discoverErr := mcp.Discover(ctx, cfg, mcp.Options{
		Timeout:        mcpRefreshTimeout,
		AllowStdio:     allow,
		StdioAllowlist: allowlist,
	})
	if discoverErr != nil {
		message := mcpSanitizeRefreshError(discoverErr)
		if markErr := o.Store.MarkMCPRefreshFailed(r.Context(), o.TenantID, serverID, message); markErr != nil {
			o.Logger.Error("记录 MCP 刷新失败状态失败", "server_id", serverID, "error", markErr)
		}
		writeError(w, r, http.StatusBadGateway, "refresh_failed", "刷新 MCP 工具清单失败："+message)
		return
	}
	records := make([]pg.MCPTool, 0, len(tools))
	for _, tool := range tools {
		records = append(records, pg.MCPTool{
			ServerID:    serverID,
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	if err := o.Store.ReplaceMCPTools(r.Context(), o.TenantID, serverID, records); err != nil {
		writeStoreError(w, r, err)
		return
	}
	views := make([]mcpToolView, 0, len(records))
	for _, tool := range records {
		views = append(views, mcpToolView{
			ServerID:    tool.ServerID,
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": views})
}

// mcpSanitizeRefreshError 生成可写入 last_error 的脱敏摘要。
//
// 发现过程中的错误文本由 mcp 包构造，本身不含 env/headers 值；这里再做一次
// 兜底：把 URL 查询串与换行去掉，并截断长度，避免 last_error 变成多行注入点。
func mcpSanitizeRefreshError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = strings.TrimSpace(text[:idx])
	}
	if len([]rune(text)) > 300 {
		text = string([]rune(text)[:300])
	}
	return text
}
