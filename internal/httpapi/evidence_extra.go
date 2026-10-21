package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B5：证据读取审计、证据导出与密钥轮换（P-B5-44..46）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册。安全约定：导出/审计都需要
// Bearer 鉴权（统一中间件）；导出时记录一条 action=export 的访问审计；正文
// 只经 evidence.Store 解密，密钥不进入日志/响应。
//
// 已知限制：既有 GET /api/v1/evidence/{id}/verify 与流量正文读取接口不在本批
// 可改文件范围内，尚未写入 evidence_access_log；后续接线后可补齐 read/verify。

const (
	// evidenceExportLimit 是证据导出包允许的最大正文总量（请求+响应，解码后）。
	evidenceExportLimit = 20 << 20
)

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/evidence/{id}/audit",
			Summary: "证据读取/校验/导出审计",
			Build:   func(o Options) http.HandlerFunc { return o.handleEvidenceAudit },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/evidence/{id}/export",
			Summary: "导出证据包（snapshot.json + 请求/响应正文 zip）",
			Build:   func(o Options) http.HandlerFunc { return o.handleEvidenceExport },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/evidence/rotate",
			Summary: "证据密钥轮换（当前返回 501，见说明）",
			Build:   func(o Options) http.HandlerFunc { return o.handleEvidenceRotate },
		},
	)
}

// handleEvidenceAudit 列出某条证据快照的访问审计（id 倒序）。
func (o Options) handleEvidenceAudit(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	snapshotID, err := pathInt64Param(r, "id", "证据")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	limit, ok := parseStrictLimit(w, r, defaultTrafficListLimit, maxTrafficListLimit)
	if !ok {
		return
	}
	entries, err := o.Store.ListEvidenceAccess(r.Context(), o.TenantID, snapshotID, limit)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if entries == nil {
		entries = []pg.EvidenceAccessLog{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": entries})
}

// handleEvidenceExport 导出证据包：snapshot.json + request.bin + response.bin。
//
// 导出前校验正文哈希/签名（GetBlob 内部校验），成功导出后写一条 export 审计；
// 审计写入失败时拒绝导出（宁可不导出，也不产生无审计的读取）。
func (o Options) handleEvidenceExport(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	if o.Evidence == nil {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable", "证据存储未就绪")
		return
	}
	snapshotID, err := pathInt64Param(r, "id", "证据")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	snapshot, err := o.Store.GetEvidenceSnapshot(r.Context(), o.TenantID, snapshotID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if snapshot.RequestSize+snapshot.ResponseSize > evidenceExportLimit {
		writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "证据正文超过 20MB，无法导出")
		return
	}

	requestBody, err := o.readTrafficBlob(r, snapshot.RequestHash, snapshot.RequestSize, "请求")
	if err != nil {
		writeTrafficBlobError(w, r, err)
		return
	}
	responseBody, err := o.readTrafficBlob(r, snapshot.ResponseHash, snapshot.ResponseSize, "响应")
	if err != nil {
		writeTrafficBlobError(w, r, err)
		return
	}
	if snapshot.RequestHash == "" && snapshot.ResponseHash == "" {
		writeError(w, r, http.StatusConflict, "conflict", "证据快照没有可导出的正文")
		return
	}

	metadata, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "生成证据元数据失败")
		return
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for _, file := range []struct {
		name string
		body []byte
	}{
		{"snapshot.json", append(metadata, '\n')},
		{"request.bin", requestBody},
		{"response.bin", responseBody},
	} {
		entry, err := zw.Create(file.name)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "生成证据包失败")
			return
		}
		if _, err := entry.Write(file.body); err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "写入证据包失败")
			return
		}
	}
	if err := zw.Close(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "关闭证据包失败")
		return
	}

	// 审计先落库再响应：没有审计记录的导出不返回给调用方。
	if _, err := o.Store.LogEvidenceAccess(r.Context(), o.TenantID, snapshotID, "export", "user"); err != nil {
		writeStoreError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"evidence-%d.zip\"", snapshotID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive.Bytes())
}

// handleEvidenceRotate 处理证据密钥轮换。
//
// 当前明确返回 501（未实现）：现有加密方案没有 key-id 前缀，轮换必须重写全部
// blob 并原子切换密钥文件，任何半途失败都会造成不可恢复的证据损坏；且 HTTP 层
// 无法安全访问 evidence.Store 的密钥/目录。推荐的上线路径见 docs/evidence.md：
// 离线双密钥窗口（旧密钥解密 + 新密钥重加密）+ 备份校验后切换。
func (o Options) handleEvidenceRotate(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotImplemented, "not_implemented",
		"证据密钥轮换尚未实现：当前方案无 key-id，重写全部 blob 需要离线双密钥窗口与备份校验；请按 docs/evidence.md 的轮换方案操作，勿在运行中直接替换密钥文件")
}
