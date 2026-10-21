package pg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/graph"
)

// B6 可观测补口（图游标分页 / Dashboard 小时趋势 / 全局活动流）。
//
// 这里的三组查询都是「读多写少、只按 id 或时间范围扫描」的聚合/分页：
//   - ListNodesPage：作战图节点分页（保留既有 ListNodes 的升序语义，外加 id 游标）；
//   - DashboardHourly：最近 24 小时按小时聚合（missions/tokens/approvals）；
//   - ListMissionEventsGlobal：跨任务的活动流（mission_events 倒序 + 游标）。
//
// 所有查询都显式携带 tenant_id；不修改既有方法签名，避免影响 B2/B3 调用方。

// DashboardHour 是 Dashboard 的「小时趋势」桶。
//
// Hour 是 RFC3339 的 UTC 整点字符串（如 2026-10-22T04:00:00Z），便于前端直接
// 作为时间轴刻度；缺失的小时也会返回一个全零桶（由 Go 侧补齐）。
type DashboardHour struct {
	Hour               string `json:"hour"`
	MissionsCreated    int64  `json:"missions_created"`
	MissionsDone       int64  `json:"missions_done"`
	Tokens             int64  `json:"tokens"`
	ApprovalsRequested int64  `json:"approvals_requested"`
}

// dashboardHourCount 是小时趋势的桶数（最近 24 小时，含当前小时）。
const dashboardHourCount = 24

// ListNodesPage 按 id 游标列出作战图节点（id 升序，afterID>0 时只取 id > afterID）。
//
// 与既有 ListNodes 的差异只有两点：显式接受 afterID、limit 上限固定为 1000
// （与 ListNodes 的内部上限一致），保证同一页的取数口径不变。
func (s *Store) ListNodesPage(ctx context.Context, tenantID, missionID, afterID int64, limit int) ([]Node, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if missionID <= 0 {
		return nil, fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	args := []any{tenantID, missionID}
	conds := []string{"tenant_id = $1", "mission_id = $2"}
	if afterID > 0 {
		args = append(args, afterID)
		conds = append(conds, fmt.Sprintf("id > $%d", len(args)))
	}
	args = append(args, limit)

	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, mission_id, kind, state, title, body, payload, created_at, updated_at
		FROM operation_nodes
		WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY id LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("列出节点失败: %w", err)
	}
	defer rows.Close()

	out := make([]Node, 0, limit)
	for rows.Next() {
		var (
			n      Node
			kind   string
			state  string
			rawPay []byte
		)
		if err := rows.Scan(&n.TenantID, &n.ID, &n.MissionID, &kind, &state, &n.Title, &n.Body,
			&rawPay, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析节点失败: %w", err)
		}
		n.Kind = graph.NodeKind(kind)
		n.State = graph.NodeState(state)
		if n.Payload, err = decodeJSON(rawPay); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历节点失败: %w", err)
	}
	return out, nil
}

// DashboardHourly 返回最近 24 小时（含当前小时）的小时聚合。
//
// 实现要点：
//   - 先在 Go 侧按 UTC 整点生成 24 个桶（缺失小时补 0），再用一条 UNION ALL
//     查询把四类指标按小时聚合回来填充，避免来回 24 次查询；
//   - 小时口径统一用 `date_trunc('hour', ts AT TIME ZONE 'UTC')`，与部署时区无关；
//   - tokens 取 llm_usage.total_tokens 之和；approvals_requested 统计审批创建数；
//     missions_done 只统计 status='done' 且已写入 finished_at 的任务。
func (s *Store) DashboardHourly(ctx context.Context, tenantID int64, now time.Time) ([]DashboardHour, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	base := now.UTC().Truncate(time.Hour)
	start := base.Add(-time.Duration(dashboardHourCount-1) * time.Hour)

	// 先生成 24 个全零桶，保证「没有数据的小时」也出现在结果里。
	buckets := make([]DashboardHour, dashboardHourCount)
	index := make(map[string]int, dashboardHourCount)
	for i := 0; i < dashboardHourCount; i++ {
		hour := start.Add(time.Duration(i) * time.Hour)
		buckets[i] = DashboardHour{Hour: hour.Format("2006-01-02T15:00:00Z")}
		index[buckets[i].Hour] = i
	}

	const hourExpr = `to_char(date_trunc('hour', %s AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:00:00Z')`
	query := `
		SELECT ` + fmt.Sprintf(hourExpr, "created_at") + ` AS bucket, 'missions_created' AS metric, count(*)::bigint AS n
		FROM missions WHERE tenant_id = $1 AND created_at >= $2 GROUP BY 1
		UNION ALL
		SELECT ` + fmt.Sprintf(hourExpr, "finished_at") + `, 'missions_done', count(*)::bigint
		FROM missions WHERE tenant_id = $1 AND status = 'done' AND finished_at >= $2 GROUP BY 1
		UNION ALL
		SELECT ` + fmt.Sprintf(hourExpr, "created_at") + `, 'tokens', COALESCE(sum(total_tokens), 0)::bigint
		FROM llm_usage WHERE tenant_id = $1 AND created_at >= $2 GROUP BY 1
		UNION ALL
		SELECT ` + fmt.Sprintf(hourExpr, "created_at") + `, 'approvals_requested', count(*)::bigint
		FROM approvals WHERE tenant_id = $1 AND created_at >= $2 GROUP BY 1`

	rows, err := s.db.pool.Query(ctx, query, tenantID, start)
	if err != nil {
		return nil, fmt.Errorf("统计小时趋势失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			bucket string
			metric string
			count  int64
		)
		if err := rows.Scan(&bucket, &metric, &count); err != nil {
			return nil, fmt.Errorf("解析小时趋势失败: %w", err)
		}
		position, ok := index[bucket]
		if !ok {
			// 边界小时可能因为 start 之前的秒级数据落进上一小时桶；忽略即可。
			continue
		}
		switch metric {
		case "missions_created":
			buckets[position].MissionsCreated = count
		case "missions_done":
			buckets[position].MissionsDone = count
		case "tokens":
			buckets[position].Tokens = count
		case "approvals_requested":
			buckets[position].ApprovalsRequested = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历小时趋势失败: %w", err)
	}
	return buckets, nil
}

// GlobalEventFilter 是全局活动流的过滤条件。
//
// MissionID=0 表示不限任务；Type 为空表示不限事件类型；BeforeID>0 表示只取
// id < BeforeID（倒序翻页的游标）。
type GlobalEventFilter struct {
	MissionID int64
	Type      string
	BeforeID  int64
	Limit     int
}

// ListMissionEventsGlobal 跨任务列出活动事件（mission_events，id 倒序）。
func (s *Store) ListMissionEventsGlobal(ctx context.Context, tenantID int64, f GlobalEventFilter) ([]MissionEvent, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit := normalizeLimit(f.Limit, defaultListLimit, maxListLimit)

	args := []any{tenantID}
	conds := []string{"tenant_id = $1"}
	if f.MissionID > 0 {
		args = append(args, f.MissionID)
		conds = append(conds, fmt.Sprintf("mission_id = $%d", len(args)))
	}
	if eventType := strings.TrimSpace(f.Type); eventType != "" {
		args = append(args, eventType)
		conds = append(conds, fmt.Sprintf("type = $%d", len(args)))
	}
	if f.BeforeID > 0 {
		args = append(args, f.BeforeID)
		conds = append(conds, fmt.Sprintf("id < $%d", len(args)))
	}
	args = append(args, limit)

	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, mission_id, type, actor, reason, payload, created_at
		FROM mission_events
		WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("列出活动事件失败: %w", err)
	}
	defer rows.Close()

	out := make([]MissionEvent, 0, limit)
	for rows.Next() {
		var (
			ev     MissionEvent
			rawPay []byte
		)
		if err := rows.Scan(&ev.TenantID, &ev.ID, &ev.MissionID, &ev.Type, &ev.Actor,
			&ev.Reason, &rawPay, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析活动事件失败: %w", err)
		}
		if ev.Payload, err = decodeJSON(rawPay); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历活动事件失败: %w", err)
	}
	return out, nil
}
