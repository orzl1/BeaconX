package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/orzl1/BeaconX/internal/domain/finding"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B2：发现、发现证据关联与复测接口。
//
// 路由由 handler.go 的 Routes()/bind() 统一注册；本文件只提供 Options 方法，
// 因此集成测试可以绕过 mux 直接调用处理器（不依赖路由是否已注册）。

// findingCreateRequest 是创建发现的请求体。
type findingCreateRequest struct {
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
	AssetID  *int64 `json:"asset_id"`
	NodeID   *int64 `json:"node_id"`
}

// findingPatchRequest 是局部更新发现的请求体。
//
// asset_id 用 json.RawMessage 承载三态语义：字段缺省=保持不变；JSON null=清空
// 资产关联；正整数=设置为该资产。这与资产接口的 parent_id 约定一致。
type findingPatchRequest struct {
	Title    *string         `json:"title"`
	Severity *string         `json:"severity"`
	Detail   *string         `json:"detail"`
	AssetID  json.RawMessage `json:"asset_id"`
}

// findingStatusRequest 是发现状态迁移的请求体。
type findingStatusRequest struct {
	Status string `json:"status"`
}

// findingEvidenceRequest 是绑定证据快照的请求体。
type findingEvidenceRequest struct {
	SnapshotID int64 `json:"snapshot_id"`
}

// retestCreateRequest 是发起复测的请求体。
type retestCreateRequest struct {
	FindingID   int64  `json:"finding_id"`
	Note        string `json:"note"`
	RequestedBy string `json:"requested_by"`
}

// retestPatchRequest 是推进复测状态的请求体。
type retestPatchRequest struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

// findingDetailResponse 是发现详情：发现字段平铺 + 证据关联列表。
type findingDetailResponse struct {
	pg.Finding
	Evidence []pg.FindingEvidence `json:"evidence"`
}

// findingPathID 解析路径中的发现标识。
func (o Options) findingPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "发现")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}

// ---------- 发现 ----------

// handleListFindings 列出任务发现，支持 ?severity= / ?status= / ?limit= 过滤。
func (o Options) handleListFindings(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	findings, err := o.Store.ListFindings(r.Context(), o.TenantID, missionID,
		q.Get("severity"), q.Get("status"), listLimitParam(r))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if findings == nil {
		findings = []pg.Finding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": findings})
}

// handleCreateFinding 在任务下创建发现（初始状态 open）。
func (o Options) handleCreateFinding(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	var req findingCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "发现标题不能为空")
		return
	}
	created, err := o.Store.CreateFinding(r.Context(), o.TenantID, missionID, pg.FindingInput{
		AssetID:  req.AssetID,
		NodeID:   req.NodeID,
		Title:    req.Title,
		Severity: finding.Severity(strings.TrimSpace(req.Severity)),
		Detail:   req.Detail,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleGetFinding 读取发现详情，并附带已绑定的证据快照列表。
func (o Options) handleGetFinding(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	findingID, ok := o.findingPathID(w, r)
	if !ok {
		return
	}
	f, err := o.Store.GetFinding(r.Context(), o.TenantID, findingID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	evidence, err := o.Store.ListFindingEvidence(r.Context(), o.TenantID, findingID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, findingDetailResponse{Finding: f, Evidence: evidence})
}

// handleUpdateFinding 局部更新发现（title/severity/detail/asset_id）。
func (o Options) handleUpdateFinding(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	findingID, ok := o.findingPathID(w, r)
	if !ok {
		return
	}
	var req findingPatchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	in := pg.FindingUpdate{Title: req.Title, Detail: req.Detail}
	if req.Severity != nil {
		severity := finding.Severity(strings.TrimSpace(*req.Severity))
		in.Severity = &severity
	}
	if len(req.AssetID) > 0 {
		raw := bytes.TrimSpace(req.AssetID)
		if bytes.Equal(raw, []byte("null")) {
			in.ClearAsset = true
		} else {
			var assetID int64
			if err := json.Unmarshal(raw, &assetID); err != nil || assetID <= 0 {
				writeError(w, r, http.StatusBadRequest, "bad_request", "asset_id 必须为正整数或 null")
				return
			}
			in.AssetID = &assetID
		}
	}
	updated, err := o.Store.UpdateFinding(r.Context(), o.TenantID, findingID, in)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleTransitionFinding 按领域状态机迁移发现状态；非法迁移返回 409。
func (o Options) handleTransitionFinding(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	findingID, ok := o.findingPathID(w, r)
	if !ok {
		return
	}
	var req findingStatusRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	to := finding.Status(strings.TrimSpace(req.Status))
	if !to.Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request", "status 不是合法的发现状态")
		return
	}
	updated, err := o.Store.TransitionFinding(r.Context(), o.TenantID, findingID, to)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ---------- 发现证据 ----------

// handleListFindingEvidence 列出发现绑定的证据快照。
func (o Options) handleListFindingEvidence(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	findingID, ok := o.findingPathID(w, r)
	if !ok {
		return
	}
	evidence, err := o.Store.ListFindingEvidence(r.Context(), o.TenantID, findingID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": evidence})
}

// handleAddFindingEvidence 把证据快照绑定到发现；成功返回 201。
func (o Options) handleAddFindingEvidence(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	findingID, ok := o.findingPathID(w, r)
	if !ok {
		return
	}
	var req findingEvidenceRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SnapshotID <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "snapshot_id 必须为正整数")
		return
	}
	if err := o.Store.AddFindingEvidence(r.Context(), o.TenantID, findingID, req.SnapshotID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"finding_id":  findingID,
		"snapshot_id": req.SnapshotID,
	})
}

// handleRemoveFindingEvidence 解除发现与证据快照的绑定；成功返回 204。
func (o Options) handleRemoveFindingEvidence(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	findingID, ok := o.findingPathID(w, r)
	if !ok {
		return
	}
	snapshotID, err := pathInt64Param(r, "snapshot_id", "证据快照")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := o.Store.RemoveFindingEvidence(r.Context(), o.TenantID, findingID, snapshotID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 复测 ----------

// handleListRetests 列出任务的复测记录。
func (o Options) handleListRetests(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	retests, err := o.Store.ListRetests(r.Context(), o.TenantID, missionID, listLimitParam(r))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if retests == nil {
		retests = []pg.Retest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"retests": retests})
}

// handleCreateRetest 为任务内的某条发现发起复测（初始状态 pending）。
func (o Options) handleCreateRetest(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	var req retestCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.FindingID <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "finding_id 必须为正整数")
		return
	}
	retest, err := o.Store.CreateRetest(r.Context(), o.TenantID, missionID,
		req.FindingID, req.Note, req.RequestedBy)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, retest)
}

// handleGetRetest 读取单条复测。
func (o Options) handleGetRetest(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	retestID, err := pathInt64Param(r, "id", "复测")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	retest, err := o.Store.GetRetest(r.Context(), o.TenantID, retestID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, retest)
}

// handlePatchRetest 推进复测状态；非法迁移返回 409。
func (o Options) handlePatchRetest(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	retestID, err := pathInt64Param(r, "id", "复测")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	var req retestPatchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	to := finding.RetestStatus(strings.TrimSpace(req.Status))
	if !to.Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request", "status 不是合法的复测状态")
		return
	}
	retest, err := o.Store.UpdateRetestStatus(r.Context(), o.TenantID, retestID, to, req.Note)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, retest)
}
