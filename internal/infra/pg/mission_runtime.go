package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/task"
)

// 任务运行期数据访问：排队、状态迁移、事件流、动作租约与恢复。

const missionColumns = `tenant_id, id, name, goal, description, status, queued, queued_at,
	started_at, finished_at, deadline_at, reason,
	max_tokens, spent_tokens, max_steps, steps_used, created_at, updated_at`

// missionScanner 抽象 pgx.Row 与 pgx.Rows 的 Scan，供复用。
type missionScanner interface {
	Scan(dest ...any) error
}

func scanMission(row missionScanner) (Mission, error) {
	var m Mission
	err := row.Scan(&m.TenantID, &m.ID, &m.Name, &m.Goal, &m.Description, &m.Status, &m.Queued,
		&m.QueuedAt, &m.StartedAt, &m.FinishedAt, &m.DeadlineAt, &m.Reason,
		&m.MaxTokens, &m.SpentTokens, &m.MaxSteps, &m.StepsUsed, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return Mission{}, err
	}
	return m, nil
}

// ListQueuedMissions 按入队时间返回排队中的任务（先进先出）。
func (s *Store) ListQueuedMissions(ctx context.Context, tenantID int64, limit int) ([]Mission, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.db.pool.Query(ctx,
		`SELECT `+missionColumns+` FROM missions
		 WHERE tenant_id = $1 AND status = 'queued' AND queued
		 ORDER BY queued_at NULLS FIRST, id
		 LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取排队任务失败: %w", err)
	}
	defer rows.Close()

	var out []Mission
	for rows.Next() {
		m, err := scanMission(rows)
		if err != nil {
			return nil, fmt.Errorf("解析排队任务失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历排队任务失败: %w", err)
	}
	return out, nil
}

// ListMissions 返回任务列表，可按状态过滤。
func (s *Store) ListMissions(ctx context.Context, tenantID int64, status string, limit int) ([]Mission, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var (
		rows pgx.Rows
		err  error
	)
	if status == "" {
		rows, err = s.db.pool.Query(ctx,
			`SELECT `+missionColumns+` FROM missions WHERE tenant_id = $1
			 ORDER BY id DESC LIMIT $2`, tenantID, limit)
	} else {
		rows, err = s.db.pool.Query(ctx,
			`SELECT `+missionColumns+` FROM missions WHERE tenant_id = $1 AND status = $2
			 ORDER BY id DESC LIMIT $3`, tenantID, status, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("读取任务列表失败: %w", err)
	}
	defer rows.Close()

	var out []Mission
	for rows.Next() {
		m, err := scanMission(rows)
		if err != nil {
			return nil, fmt.Errorf("解析任务失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历任务失败: %w", err)
	}
	return out, nil
}

// TransitionMission 以 CAS 方式迁移任务状态，并在同一事务内追加事件。
//
// 返回值 applied=false 表示当前状态不在 from 集合中（并发竞争失败），调用方应放弃本次迁移。
func (s *Store) TransitionMission(
	ctx context.Context,
	tenantID, missionID int64,
	from []task.Status,
	to task.Status,
	eventType task.EventType,
	actor, reason string,
	payload map[string]any,
) (MissionEvent, bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return MissionEvent{}, false, err
	}
	if !to.Valid() {
		return MissionEvent{}, false, fmt.Errorf("%w: 非法任务状态 %q", ErrInvalidInput, to)
	}
	if len(from) == 0 {
		return MissionEvent{}, false, fmt.Errorf("%w: 起始状态集合不能为空", ErrInvalidInput)
	}
	fromStrings := make([]string, 0, len(from))
	for _, st := range from {
		if !st.Valid() {
			return MissionEvent{}, false, fmt.Errorf("%w: 非法起始状态 %q", ErrInvalidInput, st)
		}
		// 写路径复用领域状态机：任何未被 reducer 允许的迁移都在落库前被拒绝，
		// 避免 SQL CAS 与领域规则各写一套、长期漂移。
		next, _, reduceErr := task.Reduce(st, task.Event{Type: eventType})
		if reduceErr != nil {
			return MissionEvent{}, false, fmt.Errorf("%w: %s", ErrInvalidInput, reduceErr.Error())
		}
		if next != to {
			return MissionEvent{}, false, fmt.Errorf("%w: 迁移 %s + %s 得到 %s，与目标 %s 不一致",
				ErrInvalidInput, st, eventType, next, to)
		}
		fromStrings = append(fromStrings, string(st))
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return MissionEvent{}, false, fmt.Errorf("编码事件负载失败: %w", err)
	}

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return MissionEvent{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queued := to == task.StatusQueued
	terminal := to.Terminal()
	var m Mission
	err = tx.QueryRow(ctx,
		`UPDATE missions SET
		     status = $3,
		     queued = $4,
		     queued_at = CASE WHEN $4 THEN now() ELSE queued_at END,
		     started_at = CASE WHEN $3 = 'running' AND started_at IS NULL THEN now() ELSE started_at END,
		     finished_at = CASE WHEN $5 THEN now() ELSE finished_at END,
		     reason = $6,
		     updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = ANY($7)
		 RETURNING `+missionColumns,
		tenantID, missionID, string(to), queued, terminal, reason, fromStrings).Scan(
		&m.TenantID, &m.ID, &m.Name, &m.Goal, &m.Description, &m.Status, &m.Queued,
		&m.QueuedAt, &m.StartedAt, &m.FinishedAt, &m.DeadlineAt, &m.Reason,
		&m.MaxTokens, &m.SpentTokens, &m.MaxSteps, &m.StepsUsed, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MissionEvent{}, false, nil
	}
	if err != nil {
		return MissionEvent{}, false, fmt.Errorf("迁移任务状态失败: %w", err)
	}

	ev, err := insertMissionEvent(ctx, tx, tenantID, missionID, eventType, actor, reason, rawPayload)
	if err != nil {
		return MissionEvent{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MissionEvent{}, false, fmt.Errorf("提交任务状态失败: %w", err)
	}
	return ev, true, nil
}

// AppendMissionEvent 追加一条不改变状态的事件。
func (s *Store) AppendMissionEvent(ctx context.Context, tenantID, missionID int64, eventType task.EventType, actor, reason string, payload map[string]any) (MissionEvent, error) {
	if err := checkTenant(tenantID); err != nil {
		return MissionEvent{}, err
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return MissionEvent{}, fmt.Errorf("编码事件负载失败: %w", err)
	}
	var ev MissionEvent
	err = s.db.pool.QueryRow(ctx,
		`INSERT INTO mission_events (tenant_id, mission_id, type, actor, reason, payload)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		tenantID, missionID, string(eventType), actor, reason, string(rawPayload)).
		Scan(&ev.ID, &ev.CreatedAt)
	if err != nil {
		return MissionEvent{}, fmt.Errorf("写入任务事件失败: %w", err)
	}
	ev.TenantID = tenantID
	ev.MissionID = missionID
	ev.Type = string(eventType)
	ev.Actor = actor
	ev.Reason = reason
	ev.Payload = payload
	return ev, nil
}

func insertMissionEvent(ctx context.Context, tx pgx.Tx, tenantID, missionID int64, eventType task.EventType, actor, reason string, rawPayload []byte) (MissionEvent, error) {
	ev := MissionEvent{
		TenantID:  tenantID,
		MissionID: missionID,
		Type:      string(eventType),
		Actor:     actor,
		Reason:    reason,
	}
	err := tx.QueryRow(ctx,
		`INSERT INTO mission_events (tenant_id, mission_id, type, actor, reason, payload)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		tenantID, missionID, string(eventType), actor, reason, string(rawPayload)).
		Scan(&ev.ID, &ev.CreatedAt)
	if err != nil {
		return MissionEvent{}, fmt.Errorf("写入任务事件失败: %w", err)
	}
	if len(rawPayload) > 0 {
		_ = json.Unmarshal(rawPayload, &ev.Payload)
	}
	return ev, nil
}

// ListMissionEvents 按 id 升序回放事件，afterID 为游标。
func (s *Store) ListMissionEvents(ctx context.Context, tenantID, missionID, afterID int64, limit int) ([]MissionEvent, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.pool.Query(ctx,
		`SELECT id, type, actor, reason, payload, created_at
		 FROM mission_events
		 WHERE tenant_id = $1 AND mission_id = $2 AND id > $3
		 ORDER BY id LIMIT $4`, tenantID, missionID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取任务事件失败: %w", err)
	}
	defer rows.Close()

	out := make([]MissionEvent, 0, limit)
	for rows.Next() {
		ev := MissionEvent{TenantID: tenantID, MissionID: missionID}
		var raw []byte
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.Actor, &ev.Reason, &raw, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析任务事件失败: %w", err)
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &ev.Payload)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历任务事件失败: %w", err)
	}
	return out, nil
}

// ResetRunningMissions 把上次进程遗留的 running/finalizing 任务放回队列，并记录 recovered 事件。
func (s *Store) ResetRunningMissions(ctx context.Context, tenantID int64) (int, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id FROM missions
		 WHERE tenant_id = $1 AND status IN ('running', 'finalizing')
		 FOR UPDATE`, tenantID)
	if err != nil {
		return 0, fmt.Errorf("查询遗留任务失败: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("解析遗留任务失败: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("遍历遗留任务失败: %w", err)
	}
	if len(ids) == 0 {
		return 0, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE missions SET status = 'queued', queued = true, queued_at = now(),
		     reason = '进程重启恢复', updated_at = now()
		 WHERE tenant_id = $1 AND id = ANY($2)`, tenantID, ids); err != nil {
		return 0, fmt.Errorf("重置遗留任务失败: %w", err)
	}
	payload := []byte(`{"reason":"进程重启恢复"}`)
	for _, id := range ids {
		if _, err := insertMissionEvent(ctx, tx, tenantID, id, task.EvRecovered, "system", "进程重启恢复", payload); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交恢复结果失败: %w", err)
	}
	return len(ids), nil
}

// ResetExpiredLeases 回收已过期或缺失租约的 running 动作，放回 open。
func (s *Store) ResetExpiredLeases(ctx context.Context, tenantID int64) (int, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE operation_nodes
		 SET state = 'open', lease_until = NULL, leased_by = NULL,
		     attempts = attempts + 1, updated_at = now()
		 WHERE tenant_id = $1 AND state = 'running'
		   AND (lease_until IS NULL OR lease_until < now())`, tenantID)
	if err != nil {
		return 0, fmt.Errorf("回收过期租约失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ReleaseMissionLeases 把任务下所有 running 动作放回 open。
//
// 暂停/取消/删除任务时调用：执行协程退出后，已领取但未完成且未主动释放的动作
// 不能继续占着租约，否则恢复或重启后要等租约自然过期。
func (s *Store) ReleaseMissionLeases(ctx context.Context, tenantID, missionID int64) (int, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE operation_nodes
		 SET state = 'open', lease_until = NULL, leased_by = NULL,
		     attempts = attempts + 1, updated_at = now()
		 WHERE tenant_id = $1 AND mission_id = $2 AND state = 'running'`,
		tenantID, missionID)
	if err != nil {
		return 0, fmt.Errorf("释放任务动作租约失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ClaimNextIntent 以租约方式领取一条待执行动作；没有可领取动作时返回 (nil, nil)。
func (s *Store) ClaimNextIntent(ctx context.Context, tenantID, missionID int64, owner string, ttl time.Duration) (*Node, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, fmt.Errorf("%w: 租约持有者不能为空", ErrInvalidInput)
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	const q = `
		UPDATE operation_nodes n SET
		    state = 'running',
		    lease_until = now() + ($4 * interval '1 second'),
		    leased_by = $3,
		    attempts = attempts + 1,
		    updated_at = now()
		WHERE (n.tenant_id, n.id) = (
		    SELECT tenant_id, id FROM operation_nodes
		    WHERE tenant_id = $1 AND mission_id = $2
		      AND (state = 'open' OR (state = 'running' AND (lease_until IS NULL OR lease_until < now())))
		    ORDER BY priority DESC, id
		    LIMIT 1
		    FOR UPDATE SKIP LOCKED
		)
		RETURNING tenant_id, id, mission_id, kind, state, title, body, payload, created_at, updated_at`
	var (
		n      Node
		kind   string
		state  string
		rawPay []byte
	)
	err := s.db.pool.QueryRow(ctx, q, tenantID, missionID, owner, int(ttl.Seconds())).
		Scan(&n.TenantID, &n.ID, &n.MissionID, &kind, &state, &n.Title, &n.Body, &rawPay, &n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("领取动作失败: %w", err)
	}
	n.Kind = graph.NodeKind(kind)
	n.State = graph.NodeState(state)
	if n.Payload, err = decodeJSON(rawPay); err != nil {
		return nil, err
	}
	return &n, nil
}

// HeartbeatIntent 续约；租约已丢失时返回 false。
func (s *Store) HeartbeatIntent(ctx context.Context, tenantID, nodeID int64, owner string, ttl time.Duration) (bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return false, err
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE operation_nodes
		 SET lease_until = now() + ($4 * interval '1 second'), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND state = 'running' AND leased_by = $3`,
		tenantID, nodeID, owner, int(ttl.Seconds()))
	if err != nil {
		return false, fmt.Errorf("续约动作失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// FinishIntent 结束动作并释放租约；只有当前持有者且仍在 running 时生效。
func (s *Store) FinishIntent(ctx context.Context, tenantID, nodeID int64, owner string, state graph.NodeState) (bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return false, err
	}
	switch state {
	case graph.StateDone, graph.StateExhausted, graph.StateBlocked:
	default:
		return false, fmt.Errorf("%w: 动作终态只能是 done/exhausted/blocked，收到 %q", ErrInvalidInput, state)
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE operation_nodes
		 SET state = $4, lease_until = NULL, leased_by = NULL, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND state = 'running' AND leased_by = $3`,
		tenantID, nodeID, owner, string(state))
	if err != nil {
		return false, fmt.Errorf("结束动作失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CountOpenIntents 统计任务尚未结束的动作数量。
func (s *Store) CountOpenIntents(ctx context.Context, tenantID, missionID int64) (int, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	var n int
	err := s.db.pool.QueryRow(ctx,
		`SELECT count(*) FROM operation_nodes
		 WHERE tenant_id = $1 AND mission_id = $2 AND state IN ('open', 'running')`,
		tenantID, missionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计待办动作失败: %w", err)
	}
	return n, nil
}

// DeleteMission 删除任务及其级联数据（节点、边、锚点、事件、范围、资产链接）。
func (s *Store) DeleteMission(ctx context.Context, tenantID, missionID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM missions WHERE tenant_id = $1 AND id = $2`, tenantID, missionID)
	if err != nil {
		return fmt.Errorf("删除任务失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("删除任务失败: %w", pgx.ErrNoRows)
	}
	return nil
}
