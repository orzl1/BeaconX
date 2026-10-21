package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/orzl1/BeaconX/internal/app/tools"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B4 策略规则与审批接口（P-B4-01..08）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册：NewHandler 会用 Build 工厂
// 注入 Options，因此处理器可以正常访问 Store / TenantID。
//
// 约定与 B1–B3 一致：错误统一走 errorEnvelope；规则列表带 limit（缺省 50、
// 上限 200）；所有 SQL 都带 tenant_id；写操作返回资源或 204。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/policy/rules", Summary: "策略规则列表",
			Build: func(o Options) http.HandlerFunc { return o.handlePolicyRulesList },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/policy/rules", Summary: "创建策略规则",
			Build: func(o Options) http.HandlerFunc { return o.handlePolicyRuleCreate },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/policy/rules/{id}", Summary: "策略规则详情",
			Build: func(o Options) http.HandlerFunc { return o.handlePolicyRuleGet },
		},
		FeatureRoute{
			Method: http.MethodPatch, Pattern: "/api/v1/policy/rules/{id}", Summary: "更新策略规则",
			Build: func(o Options) http.HandlerFunc { return o.handlePolicyRulePatch },
		},
		FeatureRoute{
			Method: http.MethodDelete, Pattern: "/api/v1/policy/rules/{id}", Summary: "删除策略规则",
			Build: func(o Options) http.HandlerFunc { return o.handlePolicyRuleDelete },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/policy/simulate", Summary: "策略裁决试算",
			Build: func(o Options) http.HandlerFunc { return o.handlePolicySimulate },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/approvals/{id}", Summary: "审批详情与上下文",
			Build: func(o Options) http.HandlerFunc { return o.handleApprovalGet },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/approvals/{id}/cancel", Summary: "取消待审批项",
			Build: func(o Options) http.HandlerFunc { return o.handleApprovalCancel },
		},
	)
}

// policyRuleWriteRequest 是创建/局部更新规则的请求体。
//
// 指针字段用于区分「未提供」与「提供了零值」：PATCH 时 nil 表示保持不变；
// 创建时按默认值处理（priority=0、timeout_seconds=0、enabled=true）。
type policyRuleWriteRequest struct {
	Name           *string `json:"name"`
	Tool           *string `json:"tool"`
	ArgPattern     *string `json:"arg_pattern"`
	Action         *string `json:"action"`
	Priority       *int    `json:"priority"`
	TimeoutSeconds *int    `json:"timeout_seconds"`
	Enabled        *bool   `json:"enabled"`
}

// policyRuleFilter 解析 ?enabled=&tool=&limit=。
func policyRuleFilter(w http.ResponseWriter, r *http.Request) (pg.PolicyRuleFilter, bool) {
	query := r.URL.Query()
	filter := pg.PolicyRuleFilter{Tool: strings.TrimSpace(query.Get("tool"))}

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
			return pg.PolicyRuleFilter{}, false
		}
	}

	limit := 50
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "limit 必须为正整数")
			return pg.PolicyRuleFilter{}, false
		}
		limit = n
		if limit > 200 {
			limit = 200
		}
	}
	filter.Limit = limit
	return filter, true
}

// handlePolicyRulesList 列出规则（含停用），按 priority 倒序、id 升序。
func (o Options) handlePolicyRulesList(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	filter, ok := policyRuleFilter(w, r)
	if !ok {
		return
	}
	rules, err := o.Store.ListPolicyRulesFilter(r.Context(), o.TenantID, filter)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// handlePolicyRuleCreate 创建自定义规则；名称重复返回 409。
func (o Options) handlePolicyRuleCreate(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req policyRuleWriteRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	in := pg.PolicyRuleInput{Enabled: true}
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.Tool != nil {
		in.Tool = *req.Tool
	}
	if req.ArgPattern != nil {
		in.ArgPattern = *req.ArgPattern
	}
	if req.Action != nil {
		in.Action = policy.Action(*req.Action)
	}
	if req.Priority != nil {
		in.Priority = *req.Priority
	}
	if req.TimeoutSeconds != nil {
		in.TimeoutSeconds = *req.TimeoutSeconds
	}
	if req.Enabled != nil {
		in.Enabled = *req.Enabled
	}
	rule, err := o.Store.CreatePolicyRule(r.Context(), o.TenantID, in)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// handlePolicyRuleGet 读取单条规则。
func (o Options) handlePolicyRuleGet(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	id, err := pathInt64Param(r, "id", "策略规则")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	rule, err := o.Store.GetPolicyRule(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// handlePolicyRulePatch 局部更新规则；内置 immutable 规则禁止改名称/工具/动作。
func (o Options) handlePolicyRulePatch(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	id, err := pathInt64Param(r, "id", "策略规则")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	var req policyRuleWriteRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	update := pg.PolicyRuleUpdate{
		Name:           req.Name,
		Tool:           req.Tool,
		ArgPattern:     req.ArgPattern,
		Priority:       req.Priority,
		TimeoutSeconds: req.TimeoutSeconds,
		Enabled:        req.Enabled,
	}
	if req.Action != nil {
		action := policy.Action(*req.Action)
		update.Action = &action
	}
	rule, err := o.Store.UpdatePolicyRule(r.Context(), o.TenantID, id, update)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// handlePolicyRuleDelete 删除自定义规则；内置 immutable 规则返回 409。
func (o Options) handlePolicyRuleDelete(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	id, err := pathInt64Param(r, "id", "策略规则")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := o.Store.DeletePolicyRule(r.Context(), o.TenantID, id); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// policySimulateRequest 是裁决试算请求：tool 必填，args 必须是 JSON 对象。
type policySimulateRequest struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// knownPolicyToolNames 是当前注册表里的工具名集合，用于 simulate 的「未知工具」校验。
//
// 与 main.go 构建执行器时使用同一组内置工具；M4 后续若把工具注册表搬进
// Options，这里应改为从实例读取（届时 400 的判定来源才与运行期完全一致）。
var knownPolicyToolNames = func() map[string]bool {
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

// handlePolicySimulate 用当前规则集试算一次裁决（不写库、不创建审批）。
func (o Options) handlePolicySimulate(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req policySimulateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	tool := strings.TrimSpace(req.Tool)
	if tool == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "tool 不能为空")
		return
	}
	if len(req.Args) == 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "args 必须是 JSON 对象")
		return
	}
	var argsObject map[string]json.RawMessage
	if err := json.Unmarshal(req.Args, &argsObject); err != nil || argsObject == nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "args 必须是 JSON 对象")
		return
	}
	if !knownPolicyToolNames[tool] {
		writeError(w, r, http.StatusBadRequest, "bad_request", "未知工具 "+strconv.Quote(tool))
		return
	}

	// 取启用中的规则（含 id，用于回填 rule_id）。上限 200 与列表接口一致；
	// 规则数量超过 200 时试算只覆盖前 200 条（M4 的已知限制，见文档）。
	enabledOnly := true
	rules, err := o.Store.ListPolicyRulesFilter(r.Context(), o.TenantID, pg.PolicyRuleFilter{
		Enabled: &enabledOnly, Limit: 200,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	evalRules := make([]policy.Rule, 0, len(rules))
	ruleIDByName := make(map[string]int64, len(rules))
	for _, rule := range rules {
		evalRules = append(evalRules, rule.ToPolicyRule())
		ruleIDByName[rule.Name] = rule.ID
	}

	defaultAction, err := o.defaultPolicyAction(r)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	decision, err := policy.Evaluate(tool, req.Args, evalRules, defaultAction)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "策略试算失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":    decision.Action,
		"rule_id":   ruleIDByName[decision.RuleName],
		"rule_name": decision.RuleName,
		"reason":    decision.Reason,
	})
}

// handleApprovalGet 读取审批详情与上下文回放（任务/节点）。
//
// 响应同时给出两种视图：
//   - 平铺字段（id/status/tool/args/...）：与审批列表、决策接口保持 B1 风格一致；
//   - "approval" 嵌套对象：便于前端把审批主体与 node/mission 上下文分开取用。
//
// 两种视图指向同一份数据，重复字段是为兼容不同调用方习惯，成本可忽略。
func (o Options) handleApprovalGet(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	id, err := pathInt64Param(r, "id", "审批")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	approval, err := o.Store.GetApproval(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	resp := map[string]any{}
	if raw, marshalErr := json.Marshal(approval); marshalErr == nil {
		_ = json.Unmarshal(raw, &resp)
	}
	resp["approval"] = approval
	// 上下文摘要属于「尽力回放」：节点/任务被删除时留空，而不是让详情 500。
	if node, nodeErr := o.Store.GetNode(r.Context(), o.TenantID, approval.NodeID); nodeErr == nil {
		resp["node"] = node
	} else {
		resp["node"] = nil
	}
	if mission, missionErr := o.Store.GetMission(r.Context(), o.TenantID, approval.MissionID); missionErr == nil {
		resp["mission"] = mission
	} else {
		resp["mission"] = nil
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleApprovalCancel 取消 pending 审批；非 pending 返回 409。
func (o Options) handleApprovalCancel(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	id, err := pathInt64Param(r, "id", "审批")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	// 先判 404：不存在与「已处理」是两种不同语义，不能都归为 409。
	if _, err := o.Store.GetApproval(r.Context(), o.TenantID, id); err != nil {
		writeStoreError(w, r, err)
		return
	}
	approval, applied, err := o.Store.CancelApproval(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if !applied {
		writeError(w, r, http.StatusConflict, "conflict", "审批已处理或已超时，无法取消")
		return
	}
	writeJSON(w, http.StatusOK, approval)
}
