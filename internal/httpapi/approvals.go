package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// 审批与审计接口（M4）。

type decideApprovalRequest struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note"`
}

func (o Options) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	if o.Store == nil {
		writeError(w, r, http.StatusServiceUnavailable, "runtime_unavailable", "任务运行时未就绪")
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	approvals, err := o.Store.ListApprovals(r.Context(), o.TenantID, status, limit)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": approvals})
}

func (o Options) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	if o.Store == nil {
		writeError(w, r, http.StatusServiceUnavailable, "runtime_unavailable", "任务运行时未就绪")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "审批标识不合法")
		return
	}
	var req decideApprovalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	approval, applied, err := o.Store.DecideApproval(r.Context(), o.TenantID, id, req.Approve, "user", req.Note)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	if !applied {
		writeError(w, r, http.StatusConflict, "conflict", "审批已处理或不存在")
		return
	}
	writeJSON(w, http.StatusOK, approval)
}

func (o Options) handleMissionAudit(w http.ResponseWriter, r *http.Request) {
	if o.Store == nil {
		writeError(w, r, http.StatusServiceUnavailable, "runtime_unavailable", "任务运行时未就绪")
		return
	}
	id, err := missionIDFrom(r)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	events, err := o.Store.ListAudit(r.Context(), o.TenantID, id, 200)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": events})
}
