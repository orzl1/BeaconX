package pg

import (
	"context"
	"fmt"
)

// 证据轮换的跨租户查询与通知配置重加密支持（B8）。
//
// 背景：证据 blob 目录与主密钥是进程级共享的，轮换后所有租户的 Snapshot 签名
// 都会失效，因此重签必须遍历全部租户，而不能只看当前请求的 tenant_id。

// ListAllEvidenceSnapshotsForResign 返回全部租户的证据快照（按 tenant_id,id 升序）。
//
// 单租户 v1 数据量可控；轮换是低频管理操作，这里不做分页，保证一次遍历完整。
func (s *Store) ListAllEvidenceSnapshotsForResign(ctx context.Context) ([]EvidenceSnapshot, error) {
	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, mission_id, tool, request_hash, request_size,
		       response_hash, response_size, signature, redaction, created_at
		FROM evidence_snapshots
		ORDER BY tenant_id, id`)
	if err != nil {
		return nil, fmt.Errorf("读取全租户证据快照失败: %w", err)
	}
	defer rows.Close()

	out := make([]EvidenceSnapshot, 0, 64)
	for rows.Next() {
		var snap EvidenceSnapshot
		if err := rows.Scan(&snap.TenantID, &snap.ID, &snap.MissionID, &snap.Tool,
			&snap.RequestHash, &snap.RequestSize, &snap.ResponseHash, &snap.ResponseSize,
			&snap.Signature, &snap.Redaction, &snap.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析全租户证据快照失败: %w", err)
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历全租户证据快照失败: %w", err)
	}
	return out, nil
}

// ListAllNotificationChannels 返回全部租户的通知渠道（轮换重加密用）。
func (s *Store) ListAllNotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT `+notificationChannelColumns+`
		 FROM notification_channels
		 ORDER BY tenant_id, id`)
	if err != nil {
		return nil, fmt.Errorf("读取全租户通知渠道失败: %w", err)
	}
	defer rows.Close()

	out := make([]NotificationChannel, 0, 16)
	for rows.Next() {
		var (
			channel NotificationChannel
			raw     []byte
		)
		if err := rows.Scan(&channel.TenantID, &channel.ID, &channel.Name, &channel.Type,
			&raw, &channel.Enabled, &channel.CreatedAt, &channel.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析全租户通知渠道失败: %w", err)
		}
		if channel.Config, err = decodeJSON(raw); err != nil {
			return nil, err
		}
		out = append(out, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历全租户通知渠道失败: %w", err)
	}
	return out, nil
}

// UpdateNotificationChannelStoredConfig 直接覆盖存储态 config（不做业务校验）。
//
// 仅用于轮换后把已加密的 config 换成用新密钥重加密的版本；正常 CRUD 仍必须走
// UpdateNotificationChannel（名称/类型/必填 url 校验不可绕过）。
func (s *Store) UpdateNotificationChannelStoredConfig(ctx context.Context, tenantID, channelID int64, config map[string]any) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if channelID <= 0 {
		return fmt.Errorf("%w：渠道标识必须为正数", ErrInvalidInput)
	}
	if config == nil {
		config = map[string]any{}
	}
	raw, err := marshalJSON(config)
	if err != nil {
		return fmt.Errorf("%w：config 不是合法的 JSON 对象: %v", ErrInvalidInput, err)
	}
	tag, err := s.db.pool.Exec(ctx, `
		UPDATE notification_channels SET config = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, channelID, raw)
	if err != nil {
		return fmt.Errorf("更新通知渠道加密配置失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：通知渠道不存在", ErrNotFound)
	}
	return nil
}
