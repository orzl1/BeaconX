package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// 任务范围 / 计划 / 作战图 / 节点接口（M6.2-B2）。
//
// 响应约定与 B1 一致：列表用「资源名 + 数组」包一层（不使用裸数组），创建 201、
// 删除 204，错误统一走 errorEnvelope（400/401/404/409/503）。
//
// 本文件的 handler 都只通过 Options.Store 访问数据，且每个查询都显式携带
// Options.TenantID，不做跨租户读写。

// 作战图列表的 limit 约定：缺省 500、上限 2000；非数字或非正数按缺省处理。
const (
	defaultGraphLimit = 500
	maxGraphLimit     = 2000
)

// storeNodeListMax 是 pg.Store.ListNodes 内部允许的最大条数（超出会回落到 200），
// 这里显式按它取数，避免传 2001 之类的值反而只拿到 200 条。
const storeNodeListMax = 1000

// storeEdgeListMax 是 pg.Store.ListEdges 内部允许的最大条数。
const storeEdgeListMax = 5000

// missionScopeCreateRequest 是新增任务范围条目的请求体。
type missionScopeCreateRequest struct {
	Kind           string   `json:"kind"`
	Value          string   `json:"value"`
	Note           string   `json:"note"`
	AllowedActions []string `json:"allowed_actions"`
}

// nodeStateRequest 是 PATCH 节点状态的请求体。
type nodeStateRequest struct {
	State string `json:"state"`
}

// ---------- 任务范围 ----------

// handleListMissionScope 列出任务范围（按 id 升序）；任务不存在返回 404。
func (o Options) handleListMissionScope(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionIDParam(w, r)
	if !ok || !o.requireMission(w, r, missionID) {
		return
	}
	entries, err := o.Store.ListMissionScopeEntries(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": entries})
}

// handleAddMissionScope 新增任务范围条目；同一 (kind, value) 重复返回 409。
func (o Options) handleAddMissionScope(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionIDParam(w, r)
	if !ok || !o.requireMission(w, r, missionID) {
		return
	}
	var req missionScopeCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	value := strings.TrimSpace(req.Value)
	if kind == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "范围类型不能为空")
		return
	}
	if value == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "范围值不能为空")
		return
	}

	// 先查重：AddMissionScope 是幂等 upsert（runner 需要），而 HTTP 语义要求
	// 重复创建返回 409。v1 是单租户、低频管理操作，这里的「查重 + 写入」窗口
	// 可以接受；真正的并发去重仍由 (tenant_id, mission_id, kind, value) 唯一键兜底。
	existing, err := o.Store.ListMissionScopeEntries(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	for _, e := range existing {
		if e.Kind == kind && e.Value == value {
			writeError(w, r, http.StatusConflict, "conflict", "任务范围条目已存在")
			return
		}
	}

	if err := o.Store.AddMissionScope(r.Context(), o.TenantID, missionID, []pg.ScopeEntry{{
		Kind:           kind,
		Value:          value,
		AllowedActions: req.AllowedActions,
		Note:           req.Note,
	}}); err != nil {
		writeStoreError(w, r, err)
		return
	}

	// 回读刚写入的条目，把带主键的资源返回给调用方（201）。
	entries, err := o.Store.ListMissionScopeEntries(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	for _, e := range entries {
		if e.Kind == kind && e.Value == value {
			writeJSON(w, http.StatusCreated, e)
			return
		}
	}
	writeError(w, r, http.StatusInternalServerError, "internal_error", "任务范围写入后未能读回")
}

// handleDeleteMissionScope 删除任务范围的一条条目；不存在返回 404。
func (o Options) handleDeleteMissionScope(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionIDParam(w, r)
	if !ok || !o.requireMission(w, r, missionID) {
		return
	}
	scopeID, err := pathInt64Param(r, "scope_id", "任务范围")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := o.Store.DeleteMissionScope(r.Context(), o.TenantID, missionID, scopeID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 计划 ----------

// handleMissionPlan 读取任务的计划步骤（key/status/priority/depends_on）。
func (o Options) handleMissionPlan(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionIDParam(w, r)
	if !ok || !o.requireMission(w, r, missionID) {
		return
	}
	steps, err := o.Store.ListPlanSteps(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if steps == nil {
		steps = []pg.PlanStep{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": steps})
}

// ---------- 作战图 ----------

// handleMissionGraph 一次性返回任务的节点/边/锚点，节点支持 kind/state 过滤与 limit。
func (o Options) handleMissionGraph(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionIDParam(w, r)
	if !ok || !o.requireMission(w, r, missionID) {
		return
	}

	// kind/state 过滤在领域层校验取值，非法值直接 400（不静默忽略，避免调用方
	// 以为过滤生效了却拿到全量数据）。
	query := r.URL.Query()
	var kindFilter *graph.NodeKind
	if raw := strings.TrimSpace(query.Get("kind")); raw != "" {
		kind := graph.NodeKind(raw)
		if !kind.Valid() {
			writeError(w, r, http.StatusBadRequest, "bad_request", "非法节点类型 "+strconv.Quote(raw))
			return
		}
		kindFilter = &kind
	}
	var stateFilter *graph.NodeState
	if raw := strings.TrimSpace(query.Get("state")); raw != "" {
		state := graph.NodeState(raw)
		if !state.Valid() {
			writeError(w, r, http.StatusBadRequest, "bad_request", "非法节点状态 "+strconv.Quote(raw))
			return
		}
		stateFilter = &state
	}

	limit := graphLimitParam(r)
	// 过滤在内存做：有过滤条件时尽量多取（store 上限 1000），否则按请求条数取。
	fetchLimit := limit
	if kindFilter != nil || stateFilter != nil || fetchLimit > storeNodeListMax {
		fetchLimit = storeNodeListMax
	}
	nodes, err := o.Store.ListNodes(r.Context(), o.TenantID, missionID, fetchLimit)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	filtered := make([]pg.Node, 0, len(nodes))
	for _, n := range nodes {
		if kindFilter != nil && n.Kind != *kindFilter {
			continue
		}
		if stateFilter != nil && n.State != *stateFilter {
			continue
		}
		filtered = append(filtered, n)
		if len(filtered) >= limit {
			break
		}
	}

	edges, err := o.Store.ListEdges(r.Context(), o.TenantID, missionID, storeEdgeListMax)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	anchors, err := o.Store.ListAnchors(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if edges == nil {
		edges = []pg.Edge{}
	}
	if anchors == nil {
		anchors = []pg.Anchor{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":   filtered,
		"edges":   edges,
		"anchors": anchors,
	})
}

// ---------- 节点 ----------

// handleNodeGet 读取单个作战图节点（含 payload）。
func (o Options) handleNodeGet(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	nodeID, ok := o.nodeIDParam(w, r)
	if !ok {
		return
	}
	node, err := o.Store.GetNode(r.Context(), o.TenantID, nodeID)
	if err != nil {
		writeNodeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// handleNodePatchState 更新节点状态；kind 规则不满足（如 fact→running）返回 400。
func (o Options) handleNodePatchState(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	nodeID, ok := o.nodeIDParam(w, r)
	if !ok {
		return
	}
	var req nodeStateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	state := graph.NodeState(strings.TrimSpace(req.State))
	if !state.Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request", "非法节点状态 "+strconv.Quote(req.State))
		return
	}

	// 先确认节点存在，避免把 store 层的「节点不存在」误映射成 500。
	if _, err := o.Store.GetNode(r.Context(), o.TenantID, nodeID); err != nil {
		writeNodeError(w, r, err)
		return
	}
	// SetNodeState 会按节点类型重新校验状态（graph.ValidateNode），非法迁移
	// 以 ErrInvalidInput 返回；这里原样交给统一错误映射，不放宽领域规则。
	if err := o.Store.SetNodeState(r.Context(), o.TenantID, nodeID, state); err != nil {
		writeStoreError(w, r, err)
		return
	}
	updated, err := o.Store.GetNode(r.Context(), o.TenantID, nodeID)
	if err != nil {
		writeNodeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ---------- 内部小工具 ----------

// missionIDParam 解析路径中的任务标识；非法时已写好 400。
func (o Options) missionIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "任务")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}

// nodeIDParam 解析路径中的节点标识；非法时已写好 400。
func (o Options) nodeIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "节点")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}

// requireMission 确认任务存在；不存在返回 404。任务范围/计划/作战图接口共用，
// 保证「任务不存在」的判定与错误文案只有一处。
func (o Options) requireMission(w http.ResponseWriter, r *http.Request, missionID int64) bool {
	if _, err := o.Store.GetMission(r.Context(), o.TenantID, missionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "not_found", "任务不存在")
			return false
		}
		writeStoreError(w, r, err)
		return false
	}
	return true
}

// writeNodeError 把节点读取类错误映射为 404/500；节点不存在用中文文案。
func writeNodeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "节点不存在")
		return
	}
	writeStoreError(w, r, err)
}

// graphLimitParam 读取 ?limit=：缺省 500，上限 2000；非数字或非正数按缺省处理。
func graphLimitParam(r *http.Request) int {
	limit := defaultGraphLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxGraphLimit {
		limit = maxGraphLimit
	}
	return limit
}
