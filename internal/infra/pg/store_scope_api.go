package pg

import (
	"context"
	"fmt"
	"time"
)

// 任务范围 API 补充（M6.2-B2）。
//
// mission_scope 的批量写入与「无主键列表」已在 store.go（AddMissionScope /
// ListMissionScope，供 runner 解析范围用）。HTTP 层还需要两件事：
//
//   - 带主键的条目视图：列表要展示 id，删除要按 id 定位；
//   - 删除单条范围。
//
// 因此本文件补 MissionScopeEntry + ListMissionScopeEntries + DeleteMissionScope，
// 不改动既有的 ScopeEntry/ListMissionScope，避免影响 runner 侧语义。

// MissionScopeEntry 是带主键的任务范围条目（HTTP 视图）。
type MissionScopeEntry struct {
	TenantID       int64     `json:"tenant_id"`
	ID             int64     `json:"id"`
	MissionID      int64     `json:"mission_id"`
	Kind           string    `json:"kind"`
	Value          string    `json:"value"`
	AllowedActions []string  `json:"allowed_actions"`
	Note           string    `json:"note"`
	CreatedAt      time.Time `json:"created_at"`
}

// ListMissionScopeEntries 按 id 升序读取任务范围条目（含主键）。
//
// 任务不存在时返回空列表而不是错误——是否存在由 HTTP 层单独查任务判定，
// 这样同一个 404 语义只在一个地方定义。
func (s *Store) ListMissionScopeEntries(ctx context.Context, tenantID, missionID int64) ([]MissionScopeEntry, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	const q = `
		SELECT tenant_id, id, mission_id, kind, value, allowed_actions, note, created_at
		FROM mission_scope
		WHERE tenant_id = $1 AND mission_id = $2
		ORDER BY id`
	rows, err := s.db.pool.Query(ctx, q, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("读取任务范围失败: %w", err)
	}
	defer rows.Close()

	out := []MissionScopeEntry{}
	for rows.Next() {
		var e MissionScopeEntry
		if err := rows.Scan(&e.TenantID, &e.ID, &e.MissionID, &e.Kind, &e.Value,
			&e.AllowedActions, &e.Note, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析任务范围失败: %w", err)
		}
		if e.AllowedActions == nil {
			e.AllowedActions = []string{}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历任务范围失败: %w", err)
	}
	return out, nil
}

// DeleteMissionScope 删除任务下的一条范围条目。
//
// 条目不存在（或不属于该任务/租户）返回 ErrNotFound；HTTP 层据此映射 404。
func (s *Store) DeleteMissionScope(ctx context.Context, tenantID, missionID, scopeID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM mission_scope WHERE tenant_id = $1 AND mission_id = $2 AND id = $3`,
		tenantID, missionID, scopeID)
	if err != nil {
		return fmt.Errorf("删除任务范围失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 任务范围条目 %d 不存在", ErrNotFound, scopeID)
	}
	return nil
}
