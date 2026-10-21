package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B3 洞察接口：流量详情/正文、命令历史、LLM 记录、任务用量、用量聚合与
// Dashboard。
//
// 约定与 B1/B2 一致：列表用「资源名 + 数组」包一层、详情平铺；错误统一走
// errorEnvelope；所有查询都通过 Options.Store 并显式携带 Options.TenantID。
// 正文读取依赖 Options.Evidence（未装配时返回 503 evidence_unavailable）。

const (
	defaultTrafficListLimit = 50
	maxTrafficListLimit     = 200
	defaultCommandListLimit = 50
	maxCommandListLimit     = 200
	defaultLLMRecordLimit   = 50
	maxLLMRecordLimit       = 500
	defaultSummaryDays      = 30
	maxSummaryDays          = 365
)

// ---------- 流量 ----------

// handleTrafficList 列出全局流量采集记录，支持 mission_id/tool/phase 过滤。
func (o Options) handleTrafficList(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	query := r.URL.Query()
	missionID, ok := parseOptionalQueryID(w, r, "mission_id", "mission_id")
	if !ok {
		return
	}
	phase := strings.TrimSpace(query.Get("phase"))
	if phase != "" && phase != "intent" && phase != "result" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "phase 只能是 intent 或 result")
		return
	}
	limit, ok := parseStrictLimit(w, r, defaultTrafficListLimit, maxTrafficListLimit)
	if !ok {
		return
	}
	list, err := o.Store.ListTrafficFilter(r.Context(), o.TenantID, pg.TrafficFilter{
		MissionID: missionID,
		Tool:      strings.TrimSpace(query.Get("tool")),
		Phase:     phase,
		Limit:     limit,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"traffic": list})
}

// handleTrafficGet 返回单条流量采集记录（平铺，不含正文）。
func (o Options) handleTrafficGet(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	id, err := pathInt64Param(r, "id", "流量记录")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	record, err := o.Store.GetTraffic(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// handleTrafficRequest 返回解密后的请求正文。
func (o Options) handleTrafficRequest(w http.ResponseWriter, r *http.Request) {
	o.handleTrafficBody(w, r, "request")
}

// handleTrafficResponse 返回解密后的响应正文。
func (o Options) handleTrafficResponse(w http.ResponseWriter, r *http.Request) {
	o.handleTrafficBody(w, r, "response")
}

// handleTrafficBody 读取并解密一条采集记录的请求或响应正文。
//
// 错误语义：
//   - Evidence 未装配 → 503 evidence_unavailable；
//   - 记录不存在 → 404；
//   - 该阶段没有正文（hash 为空，例如未完成的 intent 行没有响应）→ 404；
//   - blob 文件缺失、密文被篡改、密钥不匹配 → 409 tampered（GetBlob 统一返回
//     evidence.ErrTampered，这里不再区分「缺失」与「篡改」，避免给出探测信号）。
func (o Options) handleTrafficBody(w http.ResponseWriter, r *http.Request, kind string) {
	if !o.storeReady(w, r) {
		return
	}
	if o.Evidence == nil {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable", "证据存储未就绪")
		return
	}
	id, err := pathInt64Param(r, "id", "流量记录")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	record, err := o.Store.GetTraffic(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	var (
		ref  evidence.BlobRef
		text string
	)
	if kind == "request" {
		ref = evidence.BlobRef{Hash: record.RequestHash, Size: record.RequestSize}
		text = "请求"
	} else {
		ref = evidence.BlobRef{Hash: record.ResponseHash, Size: record.ResponseSize}
		text = "响应"
	}
	if ref.Hash == "" {
		writeError(w, r, http.StatusNotFound, "not_found", "该采集记录没有"+text+"正文")
		return
	}
	plain, err := o.Evidence.GetBlob(r.Context(), ref)
	if err != nil {
		if errors.Is(err, evidence.ErrTampered) {
			writeError(w, r, http.StatusConflict, "tampered", text+"正文校验失败：文件缺失、被篡改或密钥不匹配")
			return
		}
		writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(plain)
}

// handleTrafficDelete 删除一条采集行；正文 blob 与已冻结的证据快照保留。
func (o Options) handleTrafficDelete(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	id, err := pathInt64Param(r, "id", "流量记录")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := o.Store.DeleteTraffic(r.Context(), o.TenantID, id); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 命令历史 ----------

// handleCommandList 列出命令执行审计（audit_events 视图），支持 mission_id/tool/outcome。
func (o Options) handleCommandList(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	query := r.URL.Query()
	missionID, ok := parseOptionalQueryID(w, r, "mission_id", "mission_id")
	if !ok {
		return
	}
	limit, ok := parseStrictLimit(w, r, defaultCommandListLimit, maxCommandListLimit)
	if !ok {
		return
	}
	list, err := o.Store.ListAuditFilter(r.Context(), o.TenantID, pg.AuditFilter{
		MissionID: missionID,
		Tool:      strings.TrimSpace(query.Get("tool")),
		Outcome:   strings.TrimSpace(query.Get("outcome")),
		Limit:     limit,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": list})
}

// handleCommandGet 返回单条命令执行记录（AuditEvent 平铺）。
func (o Options) handleCommandGet(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	id, err := pathInt64Param(r, "id", "命令记录")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	event, err := o.Store.GetAuditEvent(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

// ---------- LLM 记录 ----------

// handleLLMRecordList 列出模型调用记录，支持 mission_id/role/model 过滤。
func (o Options) handleLLMRecordList(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	query := r.URL.Query()
	missionID, ok := parseOptionalQueryID(w, r, "mission_id", "mission_id")
	if !ok {
		return
	}
	limit, ok := parseStrictLimit(w, r, defaultLLMRecordLimit, maxLLMRecordLimit)
	if !ok {
		return
	}
	records, err := o.Store.ListLLMRecords(r.Context(), o.TenantID, pg.LLMRecordFilter{
		MissionID: missionID,
		Role:      strings.TrimSpace(query.Get("role")),
		Model:     strings.TrimSpace(query.Get("model")),
		Limit:     limit,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

// ---------- 任务用量 ----------

// handleMissionUsage 返回任务预算、调用记录与按角色/模型聚合。
func (o Options) handleMissionUsage(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, err := pathInt64Param(r, "id", "任务")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if !o.requireMission(w, r, missionID) {
		return
	}
	usage, err := o.Store.MissionUsage(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// handleLLMUsageSummary 按 model/role/day 聚合最近 days 天的用量。
func (o Options) handleLLMUsageSummary(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	query := r.URL.Query()
	groupBy := strings.TrimSpace(query.Get("group_by"))
	if groupBy == "" {
		groupBy = "model"
	}
	switch groupBy {
	case "model", "role", "day":
	default:
		writeError(w, r, http.StatusBadRequest, "bad_request", "group_by 只能是 model、role 或 day")
		return
	}
	missionID, ok := parseOptionalQueryID(w, r, "mission_id", "mission_id")
	if !ok {
		return
	}
	days := defaultSummaryDays
	if raw := strings.TrimSpace(query.Get("days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "days 必须是 1..365 的整数")
			return
		}
		days = n
		if days > maxSummaryDays {
			days = maxSummaryDays
		}
	}
	groups, err := o.Store.LLMUsageSummary(r.Context(), o.TenantID, groupBy, missionID, days)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group_by": groupBy, "groups": groups})
}

// ---------- Dashboard ----------

// handleDashboard 返回首页聚合计数。
func (o Options) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	stats, err := o.Store.Dashboard(r.Context(), o.TenantID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---------- 参数小工具 ----------

// parseOptionalQueryID 解析可选的正整数查询参数；缺省或 "0" 返回 0（不筛选），
// 其它非法值写 400 并返回 ok=false。
func parseOptionalQueryID(w http.ResponseWriter, r *http.Request, name, label string) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" || raw == "0" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", label+" 必须为正整数")
		return 0, false
	}
	return id, true
}

// parseStrictLimit 解析 limit：缺省返回 def；非数字或 <=0 返回 400；超过 max 时
// 钳制到 max（与 B1 的「钳制」语义一致，但非法输入不再静默回退默认值）。
func parseStrictLimit(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "limit 必须为正整数")
		return 0, false
	}
	if n > max {
		n = max
	}
	return n, true
}
