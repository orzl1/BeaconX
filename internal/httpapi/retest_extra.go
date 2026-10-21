package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/domain/finding"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B5：复测执行/证据列表与「从作战图节点创建发现」（P-B5-38/39/47）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册。复测执行不调用模型/工具：
// 它把「复测」定义为对关联证据的可验证复核——重算快照签名并解密正文校验哈希，
// 通过即 passed、失败即 failed、没有证据即 error。真正的自动复测执行器（跑验证
// 命令并回写结论）属于后续里程碑，这里先提供可审计的确定性语义。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/retests/{id}/run",
			Summary: "执行复测（校验关联证据完整性与签名）",
			Build:   func(o Options) http.HandlerFunc { return o.handleRetestRun },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/retests/{id}/evidence",
			Summary: "复测关联的证据快照列表",
			Build:   func(o Options) http.HandlerFunc { return o.handleRetestEvidence },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/missions/{id}/findings/from-node",
			Summary: "从作战图事实/发现节点创建发现（AI 上报入口）",
			Build:   func(o Options) http.HandlerFunc { return o.handleFindingFromNode },
		},
	)
}

// handleRetestRun 执行一次复测（pending → running → passed/failed/error）。
//
// 语义：
//   - 只有 pending 可执行，其它状态 409；
//   - 没有绑定证据 → error（无法完成复核）；
//   - 证据快照签名重算不一致、或正文解密/哈希校验失败 → failed（明确记录原因）；
//   - 全部通过 → passed。
func (o Options) handleRetestRun(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	if o.Evidence == nil {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable", "证据存储未就绪")
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
	if retest.Status != finding.RetestPending {
		writeError(w, r, http.StatusConflict, "conflict", "只有 pending 状态的复测可以执行")
		return
	}
	if _, err := o.Store.UpdateRetestStatus(r.Context(), o.TenantID, retestID, finding.RetestRunning, ""); err != nil {
		writeStoreError(w, r, err)
		return
	}

	snapshot, err := o.Store.LatestFindingEvidence(r.Context(), o.TenantID, retest.FindingID)
	if errors.Is(err, pg.ErrNotFound) {
		o.finishRetest(w, r, retestID, finding.RetestError, "无证据快照，无法执行复测")
		return
	}
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	target, note := o.verifyRetestEvidence(r, snapshot)
	// 复核属于证据访问：无论结论如何都记一条 verify 审计（best-effort，审计失败
	// 不改变复测结论，避免把可观测性问题变成业务失败）。
	_, _ = o.Store.LogEvidenceAccess(r.Context(), o.TenantID, snapshot.ID, "verify", "retest")
	o.finishRetest(w, r, retestID, target, note)
}

// verifyRetestEvidence 重算证据快照签名并校验两侧正文，返回复测结论。
func (o Options) verifyRetestEvidence(r *http.Request, snapshot pg.EvidenceSnapshot) (finding.RetestStatus, string) {
	request := evidence.BlobRef{Hash: snapshot.RequestHash, Size: snapshot.RequestSize}
	response := evidence.BlobRef{Hash: snapshot.ResponseHash, Size: snapshot.ResponseSize}

	sealed, err := o.Evidence.Seal(evidence.Snapshot{
		MissionID: snapshot.MissionID,
		Tool:      snapshot.Tool,
		Request:   request,
		Response:  response,
		Redaction: snapshot.Redaction,
		CreatedAt: snapshot.CreatedAt,
	})
	if err != nil {
		return finding.RetestFailed, "证据签名重算失败：" + err.Error()
	}
	if sealed.Signature != snapshot.Signature {
		return finding.RetestFailed, "证据签名不匹配：快照元数据被篡改或密钥不一致"
	}
	for _, side := range []struct {
		name string
		ref  evidence.BlobRef
	}{
		{"请求", request},
		{"响应", response},
	} {
		if side.ref.Hash == "" {
			continue
		}
		if _, err := o.Evidence.GetBlob(r.Context(), side.ref); err != nil {
			return finding.RetestFailed, fmt.Sprintf("%s正文校验失败：%v", side.name, err)
		}
	}
	return finding.RetestPassed, fmt.Sprintf("证据快照 #%d 的签名与正文校验通过", snapshot.ID)
}

// finishRetest 把 running 状态的复测写入终态并返回最新对象。
func (o Options) finishRetest(w http.ResponseWriter, r *http.Request, retestID int64, target finding.RetestStatus, note string) {
	updated, err := o.Store.UpdateRetestStatus(r.Context(), o.TenantID, retestID, target, note)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleRetestEvidence 列出复测关联发现绑定的证据快照。
func (o Options) handleRetestEvidence(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	retestID, err := pathInt64Param(r, "id", "复测")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	snapshots, err := o.Store.ListRetestEvidence(r.Context(), o.TenantID, retestID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if snapshots == nil {
		snapshots = []pg.EvidenceSnapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": snapshots})
}

// ---------- 从节点创建发现 ----------

type findingFromNodeRequest struct {
	NodeID   int64  `json:"node_id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
}

// handleFindingFromNode 从任务内的 fact/finding 节点创建一条发现。
//
// 这是「AI 自动上报发现」的确定性入口：节点提供事实正文，人/Agent 只需指定
// node_id 与可选标题/严重级别。同一节点重复创建返回 409；节点必须属于路径任务
// 且 kind 为 fact/finding，否则 400/404。
func (o Options) handleFindingFromNode(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	var req findingFromNodeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.NodeID <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "node_id 必须为正整数")
		return
	}
	node, err := o.Store.GetNode(r.Context(), o.TenantID, req.NodeID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if node.MissionID != missionID {
		writeError(w, r, http.StatusNotFound, "not_found", "节点不存在或不属于该任务")
		return
	}
	if node.Kind != graph.NodeFact && node.Kind != graph.NodeFinding {
		writeError(w, r, http.StatusBadRequest, "bad_request", "只有 fact/finding 节点可以创建发现")
		return
	}
	if existingID, exists, err := o.Store.FindingIDForNode(r.Context(), o.TenantID, req.NodeID); err != nil {
		writeStoreError(w, r, err)
		return
	} else if exists {
		writeError(w, r, http.StatusConflict, "conflict", fmt.Sprintf("该节点已关联发现 #%d", existingID))
		return
	}

	severity := finding.Severity(strings.TrimSpace(req.Severity))
	if severity == "" {
		severity = finding.SeverityInfo
	}
	if !severity.Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request", "severity 必须是 info/low/medium/high/critical")
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(node.Title)
	}
	if title == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "节点标题为空，必须显式提供 title")
		return
	}
	nodeID := req.NodeID
	created, err := o.Store.CreateFinding(r.Context(), o.TenantID, missionID, pg.FindingInput{
		NodeID:   &nodeID,
		Title:    title,
		Severity: severity,
		Detail:   node.Body,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}
