package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/policy"
)

// M6.2-B4 策略规则管理存储：规则 CRUD 与审批取消。
//
// 既有 store 只有「读取启用规则（Evaluate 用）」与「写入内置默认规则」；
// 管理端还需要能列出全部规则（含停用）、按 id 读取、创建/局部更新/删除，
// 以及取消 pending 审批。约定与 B1–B3 一致：
//   - 所有 SQL 都带 tenant_id；
//   - 不存在 → ErrNotFound，名称冲突 / 不可变规则被修改 → ErrConflict，
//     参数非法 → ErrInvalidInput；
//   - 列表返回非 nil 切片。

// PolicyRule 是管理端看到的策略规则（比 policy.Rule 多主键与时间戳）。
type PolicyRule struct {
	TenantID       int64         `json:"tenant_id"`
	ID             int64         `json:"id"`
	Name           string        `json:"name"`
	Tool           string        `json:"tool"`
	ArgPattern     string        `json:"arg_pattern"`
	Action         policy.Action `json:"action"`
	Priority       int           `json:"priority"`
	TimeoutSeconds int           `json:"timeout_seconds"`
	Enabled        bool          `json:"enabled"`
	Immutable      bool          `json:"immutable"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// PolicyRuleFilter 是规则列表过滤条件；Enabled 为 nil 表示不筛选启用状态。
type PolicyRuleFilter struct {
	Enabled *bool
	Tool    string
	Limit   int
}

// PolicyRuleInput 是创建规则的输入；Immutable 不允许由 API 设置，固定为 false。
type PolicyRuleInput struct {
	Name           string
	Tool           string
	ArgPattern     string
	Action         policy.Action
	Priority       int
	TimeoutSeconds int
	Enabled        bool
}

// PolicyRuleUpdate 是规则局部更新输入：nil 字段保持不变。
type PolicyRuleUpdate struct {
	Name           *string
	Tool           *string
	ArgPattern     *string
	Action         *policy.Action
	Priority       *int
	TimeoutSeconds *int
	Enabled        *bool
}

// policyRuleColumns 是 policy_rules 的读取列。
const policyRuleColumns = `tenant_id, id, name, tool, arg_pattern, action, priority,
	timeout_seconds, enabled, immutable, created_at, updated_at`

// scanPolicyRule 把一行扫描成 PolicyRule；无行时返回 ErrNotFound。
func scanPolicyRule(row pgx.Row) (PolicyRule, error) {
	var (
		r      PolicyRule
		action string
	)
	err := row.Scan(&r.TenantID, &r.ID, &r.Name, &r.Tool, &r.ArgPattern, &action,
		&r.Priority, &r.TimeoutSeconds, &r.Enabled, &r.Immutable, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyRule{}, fmt.Errorf("%w：策略规则不存在", ErrNotFound)
	}
	if err != nil {
		return PolicyRule{}, fmt.Errorf("读取策略规则失败: %w", err)
	}
	r.Action = policy.Action(action)
	return r, nil
}

// ToPolicyRule 把管理端视图转成领域规则（用于复用 policy.Rule.Validate /
// policy.Evaluate；simulate 需要既有的规则视图）。
func (r PolicyRule) ToPolicyRule() policy.Rule {
	return policy.Rule{
		Name:           r.Name,
		Tool:           r.Tool,
		ArgPattern:     r.ArgPattern,
		Action:         r.Action,
		Priority:       r.Priority,
		TimeoutSeconds: r.TimeoutSeconds,
		Enabled:        r.Enabled,
		Immutable:      r.Immutable,
	}
}

// policyRuleListLimit 把规则列表 limit 钳制到 [1, 200]；<=0 时取默认 50。
func policyRuleListLimit(limit int) int {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return limit
}

// ListPolicyRulesFilter 列出规则（含停用），按 priority 倒序、id 升序。
//
// limit 缺省 50、上限 200；enabled 为 nil 时不筛选启用状态。
func (s *Store) ListPolicyRulesFilter(ctx context.Context, tenantID int64, f PolicyRuleFilter) ([]PolicyRule, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit := policyRuleListLimit(f.Limit)

	conds := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if f.Enabled != nil {
		args = append(args, *f.Enabled)
		conds = append(conds, fmt.Sprintf("enabled = $%d", len(args)))
	}
	if tool := strings.TrimSpace(f.Tool); tool != "" {
		args = append(args, tool)
		conds = append(conds, fmt.Sprintf("tool = $%d", len(args)))
	}
	args = append(args, limit)

	q := `SELECT ` + policyRuleColumns + ` FROM policy_rules WHERE ` +
		strings.Join(conds, " AND ") +
		fmt.Sprintf(" ORDER BY priority DESC, id ASC LIMIT $%d", len(args))
	rows, err := s.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("读取策略规则列表失败: %w", err)
	}
	defer rows.Close()

	out := []PolicyRule{}
	for rows.Next() {
		r, err := scanPolicyRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历策略规则失败: %w", err)
	}
	return out, nil
}

// GetPolicyRule 按 id 读取规则；不存在返回 ErrNotFound。
func (s *Store) GetPolicyRule(ctx context.Context, tenantID, id int64) (PolicyRule, error) {
	if err := checkTenant(tenantID); err != nil {
		return PolicyRule{}, err
	}
	return scanPolicyRule(s.db.pool.QueryRow(ctx,
		`SELECT `+policyRuleColumns+` FROM policy_rules WHERE tenant_id = $1 AND id = $2`,
		tenantID, id))
}

// CreatePolicyRule 创建一条自定义规则（immutable 恒为 false）。
func (s *Store) CreatePolicyRule(ctx context.Context, tenantID int64, in PolicyRuleInput) (PolicyRule, error) {
	if err := checkTenant(tenantID); err != nil {
		return PolicyRule{}, err
	}
	rule := policy.Rule{
		Name:           strings.TrimSpace(in.Name),
		Tool:           strings.TrimSpace(in.Tool),
		ArgPattern:     in.ArgPattern,
		Action:         in.Action,
		Priority:       in.Priority,
		TimeoutSeconds: in.TimeoutSeconds,
		Enabled:        in.Enabled,
	}
	if err := rule.Validate(); err != nil {
		return PolicyRule{}, fmt.Errorf("%w：%s", ErrInvalidInput, err.Error())
	}

	var (
		r      PolicyRule
		action string
	)
	err := s.db.pool.QueryRow(ctx,
		`INSERT INTO policy_rules
		   (tenant_id, name, tool, arg_pattern, action, priority, timeout_seconds, enabled, immutable)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, false)
		 RETURNING `+policyRuleColumns,
		tenantID, rule.Name, rule.Tool, rule.ArgPattern, string(rule.Action),
		rule.Priority, rule.TimeoutSeconds, rule.Enabled).
		Scan(&r.TenantID, &r.ID, &r.Name, &r.Tool, &r.ArgPattern, &action,
			&r.Priority, &r.TimeoutSeconds, &r.Enabled, &r.Immutable, &r.CreatedAt, &r.UpdatedAt)
	if isUniqueViolation(err) {
		return PolicyRule{}, fmt.Errorf("%w：规则名 %q 已存在", ErrConflict, rule.Name)
	}
	if err != nil {
		return PolicyRule{}, fmt.Errorf("创建策略规则失败: %w", err)
	}
	r.Action = policy.Action(action)
	return r, nil
}

// UpdatePolicyRule 局部更新规则并返回更新后的行。
//
// immutable 内置规则只允许修改 enabled（人工启停）；名称、工具、动作、参数正则、
// 优先级、超时任何一项被显式提交都返回 ErrConflict——内置安全底线不能通过调低
// 优先级或被放宽参数正则绕过。Enabled 之外的字段即使提交旧值也按越权处理。
func (s *Store) UpdatePolicyRule(ctx context.Context, tenantID, id int64, in PolicyRuleUpdate) (PolicyRule, error) {
	if err := checkTenant(tenantID); err != nil {
		return PolicyRule{}, err
	}
	current, err := s.GetPolicyRule(ctx, tenantID, id)
	if err != nil {
		return PolicyRule{}, err
	}
	rule := current.ToPolicyRule()
	if in.Name != nil {
		rule.Name = strings.TrimSpace(*in.Name)
	}
	if in.Tool != nil {
		rule.Tool = strings.TrimSpace(*in.Tool)
	}
	if in.ArgPattern != nil {
		rule.ArgPattern = *in.ArgPattern
	}
	if in.Action != nil {
		rule.Action = *in.Action
	}
	if in.Priority != nil {
		rule.Priority = *in.Priority
	}
	if in.TimeoutSeconds != nil {
		rule.TimeoutSeconds = *in.TimeoutSeconds
	}
	if in.Enabled != nil {
		rule.Enabled = *in.Enabled
	}
	if err := rule.Validate(); err != nil {
		return PolicyRule{}, fmt.Errorf("%w：%s", ErrInvalidInput, err.Error())
	}
	if current.Immutable && (in.Name != nil || in.Tool != nil || in.Action != nil ||
		in.ArgPattern != nil || in.Priority != nil || in.TimeoutSeconds != nil) {
		return PolicyRule{}, fmt.Errorf("%w：内置不可变规则只允许修改 enabled", ErrConflict)
	}

	var (
		r      PolicyRule
		action string
	)
	err = s.db.pool.QueryRow(ctx,
		`UPDATE policy_rules
		 SET name = $3, tool = $4, arg_pattern = $5, action = $6, priority = $7,
		     timeout_seconds = $8, enabled = $9, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+policyRuleColumns,
		tenantID, id, rule.Name, rule.Tool, rule.ArgPattern, string(rule.Action),
		rule.Priority, rule.TimeoutSeconds, rule.Enabled).
		Scan(&r.TenantID, &r.ID, &r.Name, &r.Tool, &r.ArgPattern, &action,
			&r.Priority, &r.TimeoutSeconds, &r.Enabled, &r.Immutable, &r.CreatedAt, &r.UpdatedAt)
	if isUniqueViolation(err) {
		return PolicyRule{}, fmt.Errorf("%w：规则名 %q 已存在", ErrConflict, rule.Name)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyRule{}, fmt.Errorf("%w：策略规则不存在", ErrNotFound)
	}
	if err != nil {
		return PolicyRule{}, fmt.Errorf("更新策略规则失败: %w", err)
	}
	r.Action = policy.Action(action)
	return r, nil
}

// DeletePolicyRule 删除自定义规则；内置 immutable 规则拒绝删除（ErrConflict）。
func (s *Store) DeletePolicyRule(ctx context.Context, tenantID, id int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	current, err := s.GetPolicyRule(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if current.Immutable {
		return fmt.Errorf("%w：内置不可变规则不允许删除", ErrConflict)
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM policy_rules WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("删除策略规则失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：策略规则不存在", ErrNotFound)
	}
	return nil
}

// CancelApproval 以 CAS 方式把 pending 审批置为 cancelled。
//
// 返回 applied=false 表示审批已不是 pending（已决策/超时/取消）；调用方应先用
// GetApproval 区分 404 与 409，再调用本方法完成状态迁移。
func (s *Store) CancelApproval(ctx context.Context, tenantID, id int64) (Approval, bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return Approval{}, false, err
	}
	var (
		a   Approval
		raw []byte
	)
	err := s.db.pool.QueryRow(ctx,
		`UPDATE approvals
		 SET status = 'cancelled', decided_by = 'user',
		     note = CASE WHEN note = '' THEN '用户取消' ELSE note END,
		     decided_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'pending'
		 RETURNING tenant_id, id, mission_id, node_id, tool, args, status, note, decided_by,
		           decided_at, expires_at, created_at`,
		tenantID, id).
		Scan(&a.TenantID, &a.ID, &a.MissionID, &a.NodeID, &a.Tool, &raw, &a.Status, &a.Note,
			&a.DecidedBy, &a.DecidedAt, &a.ExpiresAt, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("取消审批失败: %w", err)
	}
	if a.Args, err = decodeJSON(raw); err != nil {
		return Approval{}, false, err
	}
	return a, true, nil
}
