package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
)

// M5：流量采集与证据快照接口。

func (o Options) handleListTraffic(w http.ResponseWriter, r *http.Request) {
	if o.Store == nil {
		writeError(w, r, http.StatusServiceUnavailable, "runtime_unavailable", "任务运行时未就绪")
		return
	}
	id, err := missionIDFrom(r)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	traffic, err := o.Store.ListTraffic(r.Context(), o.TenantID, id, 200)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"traffic": traffic})
}

func (o Options) handleCreateEvidence(w http.ResponseWriter, r *http.Request) {
	if o.Store == nil || o.Evidence == nil {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable", "证据存储未就绪")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "采集记录标识不合法")
		return
	}
	ex, err := o.Store.GetTraffic(r.Context(), o.TenantID, id)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	if ex.Phase != "result" {
		writeError(w, r, http.StatusConflict, "conflict", "采集尚未完成，不能冻结证据")
		return
	}
	req := evidence.BlobRef{Hash: ex.RequestHash, Size: ex.RequestSize}
	resp := evidence.BlobRef{Hash: ex.ResponseHash, Size: ex.ResponseSize}
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	sealed, err := o.Evidence.Seal(evidence.Snapshot{
		MissionID: ex.MissionID,
		Tool:      ex.Tool,
		Request:   req,
		Response:  resp,
		Redaction: ex.Redaction,
		CreatedAt: createdAt,
	})
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	snap, err := o.Store.CreateEvidenceSnapshot(r.Context(), o.TenantID, ex.MissionID, ex.Tool, req, resp, sealed.Signature, ex.Redaction, createdAt)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	// B8：冻结证据快照也属于证据访问（action=create），best-effort 记审计；
	// 写入失败不改变创建结果，避免审计可用性影响证据冻结。
	o.logEvidenceAccess(r, snap.ID, "create", "user")
	writeJSON(w, http.StatusCreated, snap)
}

func (o Options) handleVerifyEvidence(w http.ResponseWriter, r *http.Request) {
	if o.Store == nil || o.Evidence == nil {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable", "证据存储未就绪")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "证据标识不合法")
		return
	}
	snap, err := o.Store.GetEvidenceSnapshot(r.Context(), o.TenantID, id)
	if err != nil {
		writeMissionError(w, r, err)
		return
	}
	req := evidence.BlobRef{Hash: snap.RequestHash, Size: snap.RequestSize}
	resp := evidence.BlobRef{Hash: snap.ResponseHash, Size: snap.ResponseSize}
	// 重算签名：数据库字段被篡改时签名必然不一致。
	sealed, err := o.Evidence.Seal(evidence.Snapshot{
		MissionID: snap.MissionID,
		Tool:      snap.Tool,
		Request:   req,
		Response:  resp,
		Redaction: snap.Redaction,
		CreatedAt: snap.CreatedAt,
	})
	if err != nil || sealed.Signature != snap.Signature {
		o.logEvidenceAccess(r, snap.ID, "verify", "user")
		writeError(w, r, http.StatusConflict, "tampered", "证据签名校验失败，可能已被篡改")
		return
	}
	// 正文再校验一次内容哈希。
	if _, err := o.Evidence.GetBlob(r.Context(), req); err != nil {
		o.logEvidenceAccess(r, snap.ID, "verify", "user")
		writeError(w, r, http.StatusConflict, "tampered", "证据请求正文校验失败: "+err.Error())
		return
	}
	if _, err := o.Evidence.GetBlob(r.Context(), resp); err != nil {
		o.logEvidenceAccess(r, snap.ID, "verify", "user")
		writeError(w, r, http.StatusConflict, "tampered", "证据响应正文校验失败: "+err.Error())
		return
	}
	o.logEvidenceAccess(r, snap.ID, "verify", "user")
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "evidence_id": snap.ID, "signature": snap.Signature})
}

// logEvidenceAccess 追加一条证据访问审计（best-effort）。
//
// 审计失败不会改变业务结果：证据操作本身已经完成，把审计问题变成 5xx 只会让
// 调用方重试并产生重复记录。调用方在没有 Store 时自动跳过。
func (o Options) logEvidenceAccess(r *http.Request, snapshotID int64, action, actor string) {
	if o.Store == nil || snapshotID <= 0 {
		return
	}
	_, _ = o.Store.LogEvidenceAccess(r.Context(), o.TenantID, snapshotID, action, actor)
}
