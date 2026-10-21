package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// M6.2-B4 运行时设置存储（settings 表，迁移 0008）。
//
// 设计：settings 是租户级键值表，value 用 JSONB 保存；本文件只负责读写，
// 「键白名单 / 类型 / 范围校验 / 默认值」属于 HTTP 层（见 httpapi/settings_api.go）。
// 所有查询都带 tenant_id。

// Setting 是 settings 表的一行。
type Setting struct {
	TenantID  int64           `json:"tenant_id"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ListSettings 按 key 升序返回租户的全部设置（无记录时返回空切片）。
func (s *Store) ListSettings(ctx context.Context, tenantID int64) ([]Setting, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.pool.Query(ctx,
		`SELECT tenant_id, key, value, updated_at
		 FROM settings WHERE tenant_id = $1 ORDER BY key`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("读取设置失败: %w", err)
	}
	defer rows.Close()

	out := []Setting{}
	for rows.Next() {
		var (
			item  Setting
			value []byte
		)
		if err := rows.Scan(&item.TenantID, &item.Key, &value, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析设置失败: %w", err)
		}
		item.Value = json.RawMessage(value)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历设置失败: %w", err)
	}
	return out, nil
}

// GetSettingsMap 返回 key → 原始 JSON 值的映射（无记录时返回空 map 而不是 nil）。
func (s *Store) GetSettingsMap(ctx context.Context, tenantID int64) (map[string]json.RawMessage, error) {
	items, err := s.ListSettings(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(items))
	for _, item := range items {
		out[item.Key] = item.Value
	}
	return out, nil
}

// UpsertSettings 以事务方式批量写入设置（同 key 覆盖）；values 为空时直接返回。
func (s *Store) UpsertSettings(ctx context.Context, tenantID int64, values map[string]json.RawMessage) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	// 固定写入顺序，避免并发写入时按遍历 map 的顺序产生无意义的锁等待差异。
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启设置事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, key := range keys {
		if _, err := tx.Exec(ctx,
			`INSERT INTO settings (tenant_id, key, value)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id, key) DO UPDATE
			 SET value = EXCLUDED.value, updated_at = now()`,
			tenantID, key, string(values[key])); err != nil {
			return fmt.Errorf("写入设置 %q 失败: %w", key, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交设置事务失败: %w", err)
	}
	return nil
}
