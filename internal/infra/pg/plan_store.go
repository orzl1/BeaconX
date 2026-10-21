package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/plan"
	"github.com/orzl1/BeaconX/internal/infra/llm"
)

// 规划与计量：plan_steps / step_dependencies / llm_usage 与任务预算。

// PlanStep 是任务计划中的一步。
type PlanStep struct {
	TenantID  int64           `json:"tenant_id"`
	ID        int64           `json:"id"`
	MissionID int64           `json:"mission_id"`
	Key       string          `json:"key"`
	Title     string          `json:"title"`
	Detail    string          `json:"detail,omitempty"`
	Status    plan.StepStatus `json:"status"`
	Priority  int             `json:"priority"`
	DependsOn []string        `json:"depends_on"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// UpsertPlanSteps 幂等写入步骤与依赖；返回新插入的步骤数。
func (s *Store) UpsertPlanSteps(ctx context.Context, tenantID, missionID int64, steps []plan.Step) (int, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	if len(steps) == 0 {
		return 0, nil
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inserted := 0
	ids := map[string]int64{}
	for _, st := range steps {
		if st.Key == "" {
			return 0, fmt.Errorf("%w: 步骤 key 不能为空", ErrInvalidInput)
		}
		var id int64
		err := tx.QueryRow(ctx,
			`INSERT INTO plan_steps (tenant_id, mission_id, key, title, detail)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (tenant_id, mission_id, key) DO NOTHING
			 RETURNING id`,
			tenantID, missionID, st.Key, st.Title, st.Detail).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// 已存在：取回 id，标题/详情保持首次写入的值，避免模型反复改写计划。
			if err := tx.QueryRow(ctx,
				`SELECT id FROM plan_steps WHERE tenant_id = $1 AND mission_id = $2 AND key = $3`,
				tenantID, missionID, st.Key).Scan(&id); err != nil {
				return 0, fmt.Errorf("读取已有步骤失败: %w", err)
			}
		} else if err != nil {
			return 0, fmt.Errorf("写入计划步骤失败: %w", err)
		} else {
			inserted++
		}
		ids[st.Key] = id
	}

	for _, st := range steps {
		stepID := ids[st.Key]
		for _, depKey := range st.DependsOn {
			depID, ok := ids[depKey]
			if !ok {
				// 依赖可能由更早的规划轮次写入，需要单独查询。
				if err := tx.QueryRow(ctx,
					`SELECT id FROM plan_steps WHERE tenant_id = $1 AND mission_id = $2 AND key = $3`,
					tenantID, missionID, depKey).Scan(&depID); err != nil {
					return 0, fmt.Errorf("依赖步骤 %q 不存在: %w", depKey, err)
				}
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO step_dependencies (tenant_id, step_id, depends_on_step_id)
				 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
				tenantID, stepID, depID); err != nil {
				return 0, fmt.Errorf("写入步骤依赖失败: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交计划步骤失败: %w", err)
	}
	return inserted, nil
}

// ListPlanSteps 读取任务的计划步骤与依赖。
func (s *Store) ListPlanSteps(ctx context.Context, tenantID, missionID int64) ([]PlanStep, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.pool.Query(ctx,
		`SELECT tenant_id, id, mission_id, key, title, detail, status, priority, created_at, updated_at
		 FROM plan_steps WHERE tenant_id = $1 AND mission_id = $2
		 ORDER BY priority DESC, id`, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("读取计划步骤失败: %w", err)
	}
	defer rows.Close()

	steps := []PlanStep{}
	byID := map[int64]int{}
	for rows.Next() {
		var st PlanStep
		var status string
		if err := rows.Scan(&st.TenantID, &st.ID, &st.MissionID, &st.Key, &st.Title, &st.Detail,
			&status, &st.Priority, &st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析计划步骤失败: %w", err)
		}
		st.Status = plan.StepStatus(status)
		st.DependsOn = []string{}
		byID[st.ID] = len(steps)
		steps = append(steps, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历计划步骤失败: %w", err)
	}
	if len(steps) == 0 {
		return steps, nil
	}

	depRows, err := s.db.pool.Query(ctx,
		`SELECT d.step_id, dep.key
		 FROM step_dependencies d
		 JOIN plan_steps dep ON dep.tenant_id = d.tenant_id AND dep.id = d.depends_on_step_id
		 WHERE d.tenant_id = $1 AND dep.mission_id = $2
		 ORDER BY dep.id`, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("读取步骤依赖失败: %w", err)
	}
	defer depRows.Close()
	for depRows.Next() {
		var stepID int64
		var key string
		if err := depRows.Scan(&stepID, &key); err != nil {
			return nil, fmt.Errorf("解析步骤依赖失败: %w", err)
		}
		if idx, ok := byID[stepID]; ok {
			steps[idx].DependsOn = append(steps[idx].DependsOn, key)
		}
	}
	if err := depRows.Err(); err != nil {
		return nil, fmt.Errorf("遍历步骤依赖失败: %w", err)
	}
	return steps, nil
}

// ReadyPlanSteps 返回可执行步骤：状态 pending 且所有依赖均为 done。
func (s *Store) ReadyPlanSteps(ctx context.Context, tenantID, missionID int64) ([]PlanStep, error) {
	all, err := s.ListPlanSteps(ctx, tenantID, missionID)
	if err != nil {
		return nil, err
	}
	status := make(map[string]plan.StepStatus, len(all))
	byKey := make(map[string]PlanStep, len(all))
	for _, st := range all {
		status[st.Key] = st.Status
		byKey[st.Key] = st
	}
	var out []PlanStep
	for _, st := range all {
		if st.Status != plan.StepPending {
			continue
		}
		ready := true
		for _, dep := range st.DependsOn {
			// 与领域函数 plan.ReadySteps 一致：只有依赖 done 才算满足；
			// skipped/failed 都不满足，避免“跳过即放行”的隐式语义。
			if status[dep] != plan.StepDone {
				ready = false
				break
			}
		}
		if ready {
			out = append(out, byKey[st.Key])
		}
	}
	return out, nil
}

// SetPlanStepStatus 更新步骤状态；返回是否命中。
func (s *Store) SetPlanStepStatus(ctx context.Context, tenantID, missionID int64, key string, status plan.StepStatus) (bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return false, err
	}
	if !status.Valid() {
		return false, fmt.Errorf("%w: 非法步骤状态 %q", ErrInvalidInput, status)
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE plan_steps SET status = $4, updated_at = now()
		 WHERE tenant_id = $1 AND mission_id = $2 AND key = $3`,
		tenantID, missionID, key, string(status))
	if err != nil {
		return false, fmt.Errorf("更新步骤状态失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RecordLLMUsage 写入一次模型调用用量，并原子累加到任务 spent_tokens。
func (s *Store) RecordLLMUsage(ctx context.Context, tenantID int64, missionID int64, role, model string, usage llm.Usage) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO llm_usage (tenant_id, mission_id, role, model, prompt_tokens, completion_tokens, total_tokens)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tenantID, missionID, role, model, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens); err != nil {
		return fmt.Errorf("写入模型用量失败: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE missions SET spent_tokens = spent_tokens + $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, missionID, usage.TotalTokens); err != nil {
		return fmt.Errorf("累加任务用量失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交模型用量失败: %w", err)
	}
	return nil
}

// MissionBudget 读取任务预算与已用量。
func (s *Store) MissionBudget(ctx context.Context, tenantID, missionID int64) (maxTokens, spentTokens int64, maxSteps, stepsUsed int, err error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, 0, 0, 0, err
	}
	err = s.db.pool.QueryRow(ctx,
		`SELECT max_tokens, spent_tokens, max_steps, steps_used FROM missions
		 WHERE tenant_id = $1 AND id = $2`, tenantID, missionID).
		Scan(&maxTokens, &spentTokens, &maxSteps, &stepsUsed)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("读取任务预算失败: %w", err)
	}
	return maxTokens, spentTokens, maxSteps, stepsUsed, nil
}

// IncrementMissionSteps 累加任务已用步骤数。
func (s *Store) IncrementMissionSteps(ctx context.Context, tenantID, missionID int64, n int) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	_, err := s.db.pool.Exec(ctx,
		`UPDATE missions SET steps_used = steps_used + $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, missionID, n)
	if err != nil {
		return fmt.Errorf("累加任务步骤数失败: %w", err)
	}
	return nil
}

// SetMissionBudget 设置任务预算（0 表示不限制）。
func (s *Store) SetMissionBudget(ctx context.Context, tenantID, missionID, maxTokens int64, maxSteps int) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	_, err := s.db.pool.Exec(ctx,
		`UPDATE missions SET max_tokens = $3, max_steps = $4 WHERE tenant_id = $1 AND id = $2`,
		tenantID, missionID, maxTokens, maxSteps)
	if err != nil {
		return fmt.Errorf("设置任务预算失败: %w", err)
	}
	return nil
}
