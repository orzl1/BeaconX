package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B5：证据读取审计、证据导出与密钥轮换（P-B5-44..46）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册。安全约定：导出/审计都需要
// Bearer 鉴权（统一中间件）；导出时记录一条 action=export 的访问审计；正文
// 只经 evidence.Store 解密，密钥不进入日志/响应。
//
// 审计覆盖：export 由本文件写入；verify 由 evidence_api.go 记录；流量正文读取
// 在 insights_api.go 中按 blob 哈希反查证据快照后记录 read（读的不是证据正文时
// 没有可归属的快照，不产生记录）。

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

// evidenceRotateRequest 是密钥轮换请求体。
type evidenceRotateRequest struct {
	// NewKeyHex 是新的 32 字节主密钥（64 位十六进制字符串）。
	NewKeyHex string `json:"new_key_hex"`
}

// handleEvidenceRotate 用新主密钥重新加密全部证据 blob，并切换当前进程的内存密钥。
//
// 安全语义：
//   - 新密钥只在本次内存切换中使用，**不落盘、不写日志、不回显**；运维必须自行
//     更新 BEACONX_EVIDENCE_KEY / evidence.key 并重启，否则重启后会回到旧密钥；
//   - 轮换成功后签名子密钥同步变化，**存量 Snapshot 的 HMAC 签名会失效**（Verify
//     返回 ErrTampered），需要配套离线重签流程；轮换前必须备份 PostgreSQL 与
//     evidence 目录；
//   - 失败时 store 层尝试回滚已替换文件（best-effort）；若返回错误，必须停机并从
//     备份恢复，不能继续读写混合密钥状态。
func (o Options) handleEvidenceRotate(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	if o.Evidence == nil {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable", "证据存储未就绪")
		return
	}
	var req evidenceRotateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	raw := strings.TrimSpace(req.NewKeyHex)
	if len(raw) != 64 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "new_key_hex 必须是 64 位十六进制字符串（32 字节）")
		return
	}
	newKey, err := hex.DecodeString(raw)
	if err != nil || len(newKey) != 32 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "new_key_hex 必须是合法的 64 位十六进制字符串")
		return
	}
	// 轮换会更换证据子密钥：通知渠道的加密配置也会随之失效。轮换前先收集
	// 全部租户里已加密的渠道配置（解密成明文），轮换后用新密钥重加密回写；
	// 收集失败则直接拒绝轮换，避免「轮换成功但通知配置不可读」的半损坏状态。
	type pendingNotification struct {
		tenantID  int64
		channelID int64
		config    map[string]any
	}
	channels, chErr := o.Store.ListAllNotificationChannels(r.Context())
	if chErr != nil {
		writeError(w, r, http.StatusInternalServerError, "rotate_failed",
			fmt.Sprintf("读取通知渠道失败，未执行密钥轮换：%v", chErr))
		return
	}
	pendingNotifications := make([]pendingNotification, 0, len(channels))
	for _, channel := range channels {
		plain, encrypted, decErr := notificationPlaintextConfig(o.Evidence, channel.Config)
		if decErr != nil {
			writeError(w, r, http.StatusInternalServerError, "rotate_failed",
				fmt.Sprintf("通知渠道 #%d 配置解密失败，未执行密钥轮换：%v", channel.ID, decErr))
			return
		}
		if encrypted {
			pendingNotifications = append(pendingNotifications, pendingNotification{
				tenantID: channel.TenantID, channelID: channel.ID, config: plain,
			})
		}
	}

	rotated, err := o.Evidence.RotateKey(r.Context(), newKey)
	if err != nil {
		if errors.Is(err, evidence.ErrTampered) {
			writeError(w, r, http.StatusConflict, "tampered", fmt.Sprintf("证据密钥轮换失败（已完成 %d 个，已尝试回滚）：%v", rotated, err))
			return
		}
		writeError(w, r, http.StatusInternalServerError, "rotate_failed",
			fmt.Sprintf("证据密钥轮换失败（已完成 %d 个，请停机并从备份恢复）：%v", rotated, err))
		return
	}
	// 轮换会同时更换签名子密钥，全部租户的存量快照 HMAC 必须用新密钥重签，
	// 否则 Verify 全部失败。重签在轮换成功后同步执行；中断时返回 500 并说明
	// 已重签数量，由运维重试/从备份恢复（重签本身幂等，可重复 Seal+Update）。
	snapshots, listErr := o.Store.ListAllEvidenceSnapshotsForResign(r.Context())
	if listErr != nil {
		writeError(w, r, http.StatusInternalServerError, "rotate_resign_failed",
			fmt.Sprintf("已轮换 %d 个 blob，但读取全租户证据快照失败，签名未更新：%v；请修复数据库后重签", rotated, listErr))
		return
	}
	resigned := 0
	for _, snapshot := range snapshots {
		sealed, sealErr := o.Evidence.Seal(evidence.Snapshot{
			MissionID: snapshot.MissionID,
			Tool:      snapshot.Tool,
			Request:   evidence.BlobRef{Hash: snapshot.RequestHash, Size: snapshot.RequestSize},
			Response:  evidence.BlobRef{Hash: snapshot.ResponseHash, Size: snapshot.ResponseSize},
			Redaction: snapshot.Redaction,
			CreatedAt: snapshot.CreatedAt,
		})
		if sealErr != nil {
			writeError(w, r, http.StatusInternalServerError, "rotate_resign_failed",
				fmt.Sprintf("已轮换 %d 个 blob，但重签中断（已重签 %d/%d 个）：%v；请从备份恢复或重试重签",
					rotated, resigned, len(snapshots), sealErr))
			return
		}
		if updateErr := o.Store.UpdateEvidenceSignature(r.Context(), snapshot.TenantID, snapshot.ID, sealed.Signature); updateErr != nil {
			writeError(w, r, http.StatusInternalServerError, "rotate_resign_failed",
				fmt.Sprintf("已轮换 %d 个 blob，但重签中断（已重签 %d/%d 个）：%v；请从备份恢复或重试重签",
					rotated, resigned, len(snapshots), updateErr))
			return
		}
		// 轮换影响每条快照的签名，逐条记 rotate 审计（跨租户按各自 tenant_id）。
		_, _ = o.Store.LogEvidenceAccess(r.Context(), snapshot.TenantID, snapshot.ID, "rotate", "rotate")
		resigned++
	}

	// 通知渠道加密配置用新密钥重加密回写。
	rencrypted := 0
	for _, pending := range pendingNotifications {
		storedConfig, encErr := o.storedNotificationConfig(pending.config)
		if encErr != nil {
			writeError(w, r, http.StatusInternalServerError, "rotate_failed",
				fmt.Sprintf("已轮换 %d 个 blob、重签 %d 个快照，但通知渠道 #%d 配置重加密失败：%v",
					rotated, resigned, pending.channelID, encErr))
			return
		}
		if updateErr := o.Store.UpdateNotificationChannelStoredConfig(r.Context(),
			pending.tenantID, pending.channelID, storedConfig); updateErr != nil {
			writeError(w, r, http.StatusInternalServerError, "rotate_failed",
				fmt.Sprintf("已轮换 %d 个 blob、重签 %d 个快照，但通知渠道 #%d 配置回写失败：%v",
					rotated, resigned, pending.channelID, updateErr))
			return
		}
		rencrypted++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rotated":                  rotated,
		"resigned":                 resigned,
		"notifications_rencrypted": rencrypted,
		"new_key_hex_not_stored":   true,
		"notice": "新密钥仅用于本次进程；请更新 BEACONX_EVIDENCE_KEY 或 evidence.key 并重启；" +
			"本次已用新密钥重签全部租户证据快照并重加密通知渠道配置",
	})
}
