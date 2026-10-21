package httpapi

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/orzl1/BeaconX/internal/app/tools"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B5 Agent 定义接口（P-B5-18..23）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册，NewHandler 用 Build 工厂注入 Options。
// 本批只做定义 CRUD 与静态校验：不装配到 engine/executor（文档标 🟡 部分实现）。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/agents", Summary: "Agent 定义列表",
			Build: func(o Options) http.HandlerFunc { return o.handleAgentList },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/agents", Summary: "创建 Agent 定义",
			Build: func(o Options) http.HandlerFunc { return o.handleAgentCreate },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/agents/{key}", Summary: "Agent 详情",
			Build: func(o Options) http.HandlerFunc { return o.handleAgentGet },
		},
		FeatureRoute{
			Method: http.MethodPatch, Pattern: "/api/v1/agents/{key}", Summary: "更新 Agent 定义",
			Build: func(o Options) http.HandlerFunc { return o.handleAgentPatch },
		},
		FeatureRoute{
			Method: http.MethodDelete, Pattern: "/api/v1/agents/{key}", Summary: "删除 Agent 定义",
			Build: func(o Options) http.HandlerFunc { return o.handleAgentDelete },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/agents/{key}/test", Summary: "校验 Agent 工具与提示词（不调用 LLM）",
			Build: func(o Options) http.HandlerFunc { return o.handleAgentTest },
		},
	)
}

// agentWriteRequest 是创建/局部更新 Agent 的请求体。
// PATCH 时 nil 表示保持不变；创建时按默认值处理（role=worker、enabled=true、tools=[]）。
type agentWriteRequest struct {
	Key          *string   `json:"key"`
	Name         *string   `json:"name"`
	Role         *string   `json:"role"`
	SystemPrompt *string   `json:"system_prompt"`
	Tools        *[]string `json:"tools"`
	Model        *string   `json:"model"`
	Enabled      *bool     `json:"enabled"`
}

// knownAgentToolNames 是当前工具注册表里的工具名集合（用于 test 校验工具引用）。
// 与 main.go 构建执行器时使用同一组内置工具；工具注册表未来并入 Options 后应
// 改为从实例读取。
var knownAgentToolNames = func() map[string]bool {
	registry, err := tools.NewRegistry(tools.NewHTTPProbe(), tools.NewShellExec())
	if err != nil {
		return map[string]bool{}
	}
	names := make(map[string]bool)
	for _, def := range registry.List("") {
		names[def.Name] = true
	}
	return names
}()

// agentKeyPattern 与存储层/数据库 CHECK 一致，供 HTTP 层提前给出 400。
var agentKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// agentKeyParam 解析并校验路径中的 Agent key。
func agentKeyParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.PathValue("key"))
	if !agentKeyPattern.MatchString(key) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "Agent key 必须匹配 ^[a-z0-9][a-z0-9_-]{0,63}$")
		return "", false
	}
	return key, true
}

// handleAgentList 列出 Agent；支持 ?role=&enabled=&limit=。
func (o Options) handleAgentList(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	query := r.URL.Query()
	filter := pg.AgentFilter{Role: strings.TrimSpace(query.Get("role"))}
	if raw := strings.TrimSpace(query.Get("enabled")); raw != "" {
		switch strings.ToLower(raw) {
		case "true", "1":
			enabled := true
			filter.Enabled = &enabled
		case "false", "0":
			enabled := false
			filter.Enabled = &enabled
		default:
			writeError(w, r, http.StatusBadRequest, "bad_request", "enabled 只能是 true 或 false")
			return
		}
	}
	limit := 50
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "limit 必须为正整数")
			return
		}
		limit = n
		if limit > 200 {
			limit = 200
		}
	}
	filter.Limit = limit
	agents, err := o.Store.ListAgents(r.Context(), o.TenantID, filter)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// handleAgentCreate 创建 Agent；key 重复返回 409。
func (o Options) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req agentWriteRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Key == nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "key 不能为空")
		return
	}
	in := pg.AgentInput{Key: *req.Key, Role: pg.AgentRoleWorker, Enabled: true, Tools: []string{}}
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.Role != nil {
		in.Role = *req.Role
	}
	if req.SystemPrompt != nil {
		in.SystemPrompt = *req.SystemPrompt
	}
	if req.Tools != nil {
		in.Tools = *req.Tools
	}
	if req.Model != nil {
		in.Model = *req.Model
	}
	if req.Enabled != nil {
		in.Enabled = *req.Enabled
	}
	agent, err := o.Store.CreateAgent(r.Context(), o.TenantID, in)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, agent)
}

// handleAgentGet 读取 Agent 详情。
func (o Options) handleAgentGet(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	key, ok := agentKeyParam(w, r)
	if !ok {
		return
	}
	agent, err := o.Store.GetAgent(r.Context(), o.TenantID, key)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// handleAgentPatch 局部更新 Agent；key 不可修改（传了不同值返回 400）。
func (o Options) handleAgentPatch(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	key, ok := agentKeyParam(w, r)
	if !ok {
		return
	}
	var req agentWriteRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Key != nil && strings.TrimSpace(*req.Key) != key {
		writeError(w, r, http.StatusBadRequest, "bad_request", "Agent key 不可修改")
		return
	}
	update := pg.AgentUpdate{
		Name:         req.Name,
		Role:         req.Role,
		SystemPrompt: req.SystemPrompt,
		Tools:        req.Tools,
		Model:        req.Model,
		Enabled:      req.Enabled,
	}
	agent, err := o.Store.UpdateAgent(r.Context(), o.TenantID, key, update)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// handleAgentDelete 删除 Agent。
func (o Options) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	key, ok := agentKeyParam(w, r)
	if !ok {
		return
	}
	if err := o.Store.DeleteAgent(r.Context(), o.TenantID, key); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentTestResponse 是 Agent 静态校验结果（不调用 LLM）。
type agentTestResponse struct {
	Key          string   `json:"key"`
	ValidTools   []string `json:"valid_tools"`
	UnknownTools []string `json:"unknown_tools"`
	Warnings     []string `json:"warnings"`
}

// handleAgentTest 校验 Agent 的工具引用与基本配置，返回分项结果。
//
// 设计取舍：未知工具**不返回 400**，而是 200 + unknown_tools + warnings。
// 原因：定义阶段允许先引用尚未注册的工具（工具注册表可能随版本变化），
// 让前端在编辑器里提示；真正的执行期拦截由 executor/策略门负责。
func (o Options) handleAgentTest(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	key, ok := agentKeyParam(w, r)
	if !ok {
		return
	}
	agent, err := o.Store.GetAgent(r.Context(), o.TenantID, key)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	resp := agentTestResponse{
		Key:          agent.Key,
		ValidTools:   []string{},
		UnknownTools: []string{},
		Warnings:     []string{},
	}
	for _, name := range agent.Tools {
		if knownAgentToolNames[name] {
			resp.ValidTools = append(resp.ValidTools, name)
			continue
		}
		resp.UnknownTools = append(resp.UnknownTools, name)
		resp.Warnings = append(resp.Warnings, "未知工具："+name)
	}
	if strings.TrimSpace(agent.SystemPrompt) == "" {
		resp.Warnings = append(resp.Warnings, "system_prompt 为空")
	}
	if strings.TrimSpace(agent.Model) == "" {
		resp.Warnings = append(resp.Warnings, "未指定 model（将使用全局默认模型）")
	}
	if !agent.Enabled {
		resp.Warnings = append(resp.Warnings, "Agent 处于停用状态")
	}
	writeJSON(w, http.StatusOK, resp)
}
