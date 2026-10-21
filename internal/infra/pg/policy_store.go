package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/policy"
)

// 策略规则、审批与审计的持久化。

// Approval 是一条待人工决策的工具调用。
type Approval struct {
	TenantID  int64          `json:"tenant_id"`
	ID        int64          `json:"id"`
	MissionID int64          `json:"mission_id"`
	NodeID    int64          `json:"node_id"`
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args"`
	Status    string         `json:"status"`
	Note      string         `json:"note"`
	DecidedBy string         `json:"decided_by"`
	DecidedAt *time.Time     `json:"decided_at,omitempty"`
	ExpiresAt time.Time      `json:"expires_at"`
	CreatedAt time.Time      `json:"created_at"`
}

// AuditEvent 是一次工具调用的策略与执行记录。
type AuditEvent struct {
	TenantID   int64     `json:"tenant_id"`
	ID         int64     `json:"id"`
	MissionID  int64     `json:"mission_id"`
	NodeID     *int64    `json:"node_id,omitempty"`
	Tool       string    `json:"tool"`
	Action     string    `json:"action"`
	RuleName   string    `json:"rule_name"`
	ArgsDigest string    `json:"args_digest"`
	Outcome    string    `json:"outcome"`
	Detail     string    `json:"detail"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}

// EnsurePolicyRules 写入内置规则（按 name 幂等），不会覆盖用户改动。
func (s *Store) EnsurePolicyRules(ctx context.Context, tenantID int64, rules []policy.Rule) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if len(rules) == 0 {
		return nil
	}
	const q = `
		INSERT INTO policy_rules (tenant_id, name, tool, arg_pattern, action, priority, timeout_seconds, enabled, immutable)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (tenant_id, name) DO NOTHING`
	for _, r := range rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("内置策略规则不合法: %w", err)
		}
		if _, err := s.db.pool.Exec(ctx, q, tenantID, r.Name, r.Tool, r.ArgPattern,
			string(r.Action), r.Priority, r.TimeoutSeconds, r.Enabled, r.Immutable); err != nil {
			return fmt.Errorf("写入内置策略规则失败: %w", err)
		}
	}
	return nil
}

// ListPolicyRules 读取启用中的策略规则，按优先级倒序。
func (s *Store) ListPolicyRules(ctx context.Context, tenantID int64) ([]policy.Rule, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.pool.Query(ctx,
		`SELECT name, tool, arg_pattern, action, priority, timeout_seconds, enabled, immutable
		 FROM policy_rules WHERE tenant_id = $1 AND enabled = true
		 ORDER BY priority DESC, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("读取策略规则失败: %w", err)
	}
	defer rows.Close()

	var out []policy.Rule
	for rows.Next() {
		var r policy.Rule
		var action string
		if err := rows.Scan(&r.Name, &r.Tool, &r.ArgPattern, &action, &r.Priority,
			&r.TimeoutSeconds, &r.Enabled, &r.Immutable); err != nil {
			return nil, fmt.Errorf("解析策略规则失败: %w", err)
		}
		r.Action = policy.Action(action)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历策略规则失败: %w", err)
	}
	return out, nil
}

// CreateApproval 为一次 ask 决策创建待审批项。
func (s *Store) CreateApproval(ctx context.Context, tenantID, missionID, nodeID int64, tool string, args []byte, timeout time.Duration) (Approval, error) {
	if err := checkTenant(tenantID); err != nil {
		return Approval{}, err
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	var (
		a   Approval
		raw []byte
	)
	err := s.db.pool.QueryRow(ctx,
		`INSERT INTO approvals (tenant_id, mission_id, node_id, tool, args, expires_at)
		 VALUES ($1, $2, $3, $4, $5, now() + ($6 * interval '1 second'))
		 RETURNING tenant_id, id, mission_id, node_id, tool, args, status, note, decided_by,
		           decided_at, expires_at, created_at`,
		tenantID, missionID, nodeID, tool, string(args), int(timeout.Seconds())).
		Scan(&a.TenantID, &a.ID, &a.MissionID, &a.NodeID, &a.Tool, &raw, &a.Status, &a.Note,
			&a.DecidedBy, &a.DecidedAt, &a.ExpiresAt, &a.CreatedAt)
	if err != nil {
		return Approval{}, fmt.Errorf("创建审批失败: %w", err)
	}
	if a.Args, err = decodeJSON(raw); err != nil {
		return Approval{}, err
	}
	return a, nil
}

// GetApproval 读取审批。
func (s *Store) GetApproval(ctx context.Context, tenantID, id int64) (Approval, error) {
	if err := checkTenant(tenantID); err != nil {
		return Approval{}, err
	}
	var (
		a   Approval
		raw []byte
	)
	err := s.db.pool.QueryRow(ctx,
		`SELECT tenant_id, id, mission_id, node_id, tool, args, status, note, decided_by,
		        decided_at, expires_at, created_at
		 FROM approvals WHERE tenant_id = $1 AND id = $2`, tenantID, id).
		Scan(&a.TenantID, &a.ID, &a.MissionID, &a.NodeID, &a.Tool, &raw, &a.Status, &a.Note,
			&a.DecidedBy, &a.DecidedAt, &a.ExpiresAt, &a.CreatedAt)
	if err != nil {
		return Approval{}, fmt.Errorf("读取审批失败: %w", err)
	}
	if a.Args, err = decodeJSON(raw); err != nil {
		return Approval{}, err
	}
	return a, nil
}

// DecideApproval 以 CAS 方式决策待审批项；只有 pending 状态可被决策。
func (s *Store) DecideApproval(ctx context.Context, tenantID, id int64, approve bool, decidedBy, note string) (Approval, bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return Approval{}, false, err
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	var (
		a   Approval
		raw []byte
	)
	err := s.db.pool.QueryRow(ctx,
		`UPDATE approvals SET status = $3, decided_by = $4, note = $5, decided_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'pending'
		 RETURNING tenant_id, id, mission_id, node_id, tool, args, status, note, decided_by,
		           decided_at, expires_at, created_at`,
		tenantID, id, status, decidedBy, note).
		Scan(&a.TenantID, &a.ID, &a.MissionID, &a.NodeID, &a.Tool, &raw, &a.Status, &a.Note,
			&a.DecidedBy, &a.DecidedAt, &a.ExpiresAt, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("决策审批失败: %w", err)
	}
	if a.Args, err = decodeJSON(raw); err != nil {
		return Approval{}, false, err
	}
	return a, true, nil
}

// ExpireApproval 把已过期的 pending 审批置为 timeout。
func (s *Store) ExpireApproval(ctx context.Context, tenantID, id int64) (bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return false, err
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE approvals SET status = 'timeout', decided_at = now(), note = '审批超时'
		 WHERE tenant_id = $1 AND id = $2 AND status = 'pending' AND expires_at < now()`,
		tenantID, id)
	if err != nil {
		return false, fmt.Errorf("审批超时处理失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListApprovals 列出审批，可按状态过滤。
func (s *Store) ListApprovals(ctx context.Context, tenantID int64, status string, limit int) ([]Approval, error) {
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
			`SELECT tenant_id, id, mission_id, node_id, tool, args, status, note, decided_by,
			        decided_at, expires_at, created_at
			 FROM approvals WHERE tenant_id = $1 ORDER BY id DESC LIMIT $2`, tenantID, limit)
	} else {
		rows, err = s.db.pool.Query(ctx,
			`SELECT tenant_id, id, mission_id, node_id, tool, args, status, note, decided_by,
			        decided_at, expires_at, created_at
			 FROM approvals WHERE tenant_id = $1 AND status = $2 ORDER BY id DESC LIMIT $3`,
			tenantID, status, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("读取审批列表失败: %w", err)
	}
	defer rows.Close()

	out := []Approval{}
	for rows.Next() {
		var (
			a   Approval
			raw []byte
		)
		if err := rows.Scan(&a.TenantID, &a.ID, &a.MissionID, &a.NodeID, &a.Tool, &raw, &a.Status,
			&a.Note, &a.DecidedBy, &a.DecidedAt, &a.ExpiresAt, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析审批失败: %w", err)
		}
		if a.Args, err = decodeJSON(raw); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历审批失败: %w", err)
	}
	return out, nil
}

// InsertAudit 写入一条工具调用审计。
func (s *Store) InsertAudit(ctx context.Context, tenantID, missionID int64, nodeID *int64, tool string, action policy.Action, ruleName string, args []byte, outcome, detail string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	digest := sha256.Sum256(args)
	_, err := s.db.pool.Exec(ctx,
		`INSERT INTO audit_events (tenant_id, mission_id, node_id, tool, action, rule_name, args_digest, outcome, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tenantID, missionID, nodeID, tool, string(action), ruleName, hex.EncodeToString(digest[:]), outcome, detail)
	if err != nil {
		return fmt.Errorf("写入审计失败: %w", err)
	}
	return nil
}

// ListAudit 读取任务审计。
func (s *Store) ListAudit(ctx context.Context, tenantID, missionID int64, limit int) ([]AuditEvent, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.pool.Query(ctx,
		`SELECT tenant_id, id, mission_id, node_id, tool, action, rule_name, args_digest, outcome, detail, actor, created_at
		 FROM audit_events WHERE tenant_id = $1 AND mission_id = $2 ORDER BY id DESC LIMIT $3`,
		tenantID, missionID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取审计失败: %w", err)
	}
	defer rows.Close()

	out := []AuditEvent{}
	for rows.Next() {
		var a AuditEvent
		if err := rows.Scan(&a.TenantID, &a.ID, &a.MissionID, &a.NodeID, &a.Tool, &a.Action,
			&a.RuleName, &a.ArgsDigest, &a.Outcome, &a.Detail, &a.Actor, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析审计失败: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历审计失败: %w", err)
	}
	return out, nil
}
