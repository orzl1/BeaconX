package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// M6.2-B5 Agent 定义存储（迁移 0012）。
//
// 约定与 B1–B4 一致：
//   - 所有 SQL 都带 tenant_id；
//   - 「不存在」返回 ErrNotFound，「key 重复」返回 ErrConflict，「参数非法」
//     返回 ErrInvalidInput；
//   - 列表返回非 nil 切片，tools 为空时输出 []。
//
// 注意：本批只做定义 CRUD，不接入 engine/executor（文档标 🟡 部分实现）。

// Agent 是一个 Agent 定义。
type Agent struct {
	TenantID     int64     `json:"tenant_id"`
	ID           int64     `json:"id"`
	Key          string    `json:"key"`
	Name         string    `json:"name"`
	Role         string    `json:"role"`
	SystemPrompt string    `json:"system_prompt"`
	Tools        []string  `json:"tools"`
	Model        string    `json:"model"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Agent 角色（与数据库 CHECK 一致）。
const (
	AgentRolePlanner  = "planner"
	AgentRoleWorker   = "worker"
	AgentRoleRetester = "retester"
	AgentRoleMain     = "main"
)

// ValidAgentRole 报告 role 是否是已知的 Agent 角色。
func ValidAgentRole(role string) bool {
	switch role {
	case AgentRolePlanner, AgentRoleWorker, AgentRoleRetester, AgentRoleMain:
		return true
	}
	return false
}

// agentKeyPattern 与 0012 迁移里的 CHECK 一致。
var agentKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// AgentInput 是创建 Agent 的输入。
type AgentInput struct {
	Key          string
	Name         string
	Role         string
	SystemPrompt string
	Tools        []string
	Model        string
	Enabled      bool
}

// AgentFilter 是 Agent 列表过滤条件；Role 为空、Enabled 为 nil 表示不筛选。
type AgentFilter struct {
	Role    string
	Enabled *bool
	Limit   int
}

// AgentUpdate 是 Agent 局部更新输入：nil 字段保持不变。
type AgentUpdate struct {
	Name         *string
	Role         *string
	SystemPrompt *string
	Tools        *[]string
	Model        *string
	Enabled      *bool
}

const agentColumns = `tenant_id, id, key, name, role, system_prompt, tools, model,
	enabled, created_at, updated_at`

// cleanAgentTools 校验并规范化工具名列表：去空、去重、保序。
func cleanAgentTools(tools []string) ([]string, error) {
	out := make([]string, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, raw := range tools {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("%w: 工具名不能为空", ErrInvalidInput)
		}
		if len(name) > 128 {
			return nil, fmt.Errorf("%w: 工具名 %q 过长", ErrInvalidInput, name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) > 64 {
		return nil, fmt.Errorf("%w: 工具数量不能超过 64 个", ErrInvalidInput)
	}
	return out, nil
}

// validateAgentInput 校验 Agent 定义的公共约束。
func validateAgentInput(key, role string) error {
	if !agentKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: key 必须匹配 %s", ErrInvalidInput, agentKeyPattern.String())
	}
	if role == "" {
		role = AgentRoleWorker
	}
	if !ValidAgentRole(role) {
		return fmt.Errorf("%w: 非法 Agent 角色 %q（允许 planner/worker/retester/main）", ErrInvalidInput, role)
	}
	return nil
}

// scanAgent 把一行扫描成 Agent。
func scanAgent(row pgx.Row) (Agent, error) {
	var (
		a      Agent
		tools  []byte
		rawErr error
	)
	err := row.Scan(&a.TenantID, &a.ID, &a.Key, &a.Name, &a.Role, &a.SystemPrompt,
		&tools, &a.Model, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, fmt.Errorf("%w：Agent 不存在", ErrNotFound)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("读取 Agent 失败: %w", err)
	}
	a.Tools = []string{}
	if len(tools) > 0 {
		if rawErr = json.Unmarshal(tools, &a.Tools); rawErr != nil {
			return Agent{}, fmt.Errorf("解析 Agent 工具列表失败: %w", rawErr)
		}
		if a.Tools == nil {
			a.Tools = []string{}
		}
	}
	return a, nil
}

// ListAgents 列出 Agent，按 id 升序；支持 role / enabled 过滤。
func (s *Store) ListAgents(ctx context.Context, tenantID int64, f AgentFilter) ([]Agent, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	conds := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if role := strings.TrimSpace(f.Role); role != "" {
		if !ValidAgentRole(role) {
			return nil, fmt.Errorf("%w: 非法 Agent 角色 %q", ErrInvalidInput, role)
		}
		args = append(args, role)
		conds = append(conds, fmt.Sprintf("role = $%d", len(args)))
	}
	if f.Enabled != nil {
		args = append(args, *f.Enabled)
		conds = append(conds, fmt.Sprintf("enabled = $%d", len(args)))
	}
	args = append(args, limit)

	q := `SELECT ` + agentColumns + ` FROM agents WHERE ` + strings.Join(conds, " AND ") +
		fmt.Sprintf(" ORDER BY id ASC LIMIT $%d", len(args))
	rows, err := s.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("读取 Agent 列表失败: %w", err)
	}
	defer rows.Close()

	out := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 Agent 失败: %w", err)
	}
	return out, nil
}

// GetAgent 按 key 读取 Agent；不存在返回 ErrNotFound。
func (s *Store) GetAgent(ctx context.Context, tenantID int64, key string) (Agent, error) {
	if err := checkTenant(tenantID); err != nil {
		return Agent{}, err
	}
	return scanAgent(s.db.pool.QueryRow(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE tenant_id = $1 AND key = $2`,
		tenantID, strings.TrimSpace(key)))
}

// CreateAgent 创建 Agent；key 重复返回 ErrConflict。
func (s *Store) CreateAgent(ctx context.Context, tenantID int64, in AgentInput) (Agent, error) {
	if err := checkTenant(tenantID); err != nil {
		return Agent{}, err
	}
	key := strings.TrimSpace(in.Key)
	role := strings.TrimSpace(in.Role)
	if role == "" {
		role = AgentRoleWorker
	}
	if err := validateAgentInput(key, role); err != nil {
		return Agent{}, err
	}
	tools, err := cleanAgentTools(in.Tools)
	if err != nil {
		return Agent{}, err
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return Agent{}, fmt.Errorf("编码工具列表失败: %w", err)
	}

	var a Agent
	var rawsTools []byte
	err = s.db.pool.QueryRow(ctx,
		`INSERT INTO agents (tenant_id, key, name, role, system_prompt, tools, model, enabled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING `+agentColumns,
		tenantID, key, strings.TrimSpace(in.Name), role, in.SystemPrompt,
		string(toolsJSON), strings.TrimSpace(in.Model), in.Enabled).
		Scan(&a.TenantID, &a.ID, &a.Key, &a.Name, &a.Role, &a.SystemPrompt, &rawsTools,
			&a.Model, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if isUniqueViolation(err) {
		return Agent{}, fmt.Errorf("%w：Agent key %q 已存在", ErrConflict, key)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("创建 Agent 失败: %w", err)
	}
	a.Tools = tools
	return a, nil
}

// UpdateAgent 局部更新 Agent；key 不可修改。
func (s *Store) UpdateAgent(ctx context.Context, tenantID int64, key string, up AgentUpdate) (Agent, error) {
	if err := checkTenant(tenantID); err != nil {
		return Agent{}, err
	}
	current, err := s.GetAgent(ctx, tenantID, key)
	if err != nil {
		return Agent{}, err
	}
	if up.Name != nil {
		current.Name = strings.TrimSpace(*up.Name)
	}
	if up.Role != nil {
		current.Role = strings.TrimSpace(*up.Role)
	}
	if up.SystemPrompt != nil {
		current.SystemPrompt = *up.SystemPrompt
	}
	if up.Model != nil {
		current.Model = strings.TrimSpace(*up.Model)
	}
	if up.Enabled != nil {
		current.Enabled = *up.Enabled
	}
	if up.Tools != nil {
		tools, err := cleanAgentTools(*up.Tools)
		if err != nil {
			return Agent{}, err
		}
		current.Tools = tools
	}
	if err := validateAgentInput(current.Key, current.Role); err != nil {
		return Agent{}, err
	}
	toolsJSON, err := json.Marshal(current.Tools)
	if err != nil {
		return Agent{}, fmt.Errorf("编码工具列表失败: %w", err)
	}

	var a Agent
	var rawsTools []byte
	err = s.db.pool.QueryRow(ctx,
		`UPDATE agents
		 SET name = $3, role = $4, system_prompt = $5, tools = $6, model = $7,
		     enabled = $8, updated_at = now()
		 WHERE tenant_id = $1 AND key = $2
		 RETURNING `+agentColumns,
		tenantID, current.Key, current.Name, current.Role, current.SystemPrompt,
		string(toolsJSON), current.Model, current.Enabled).
		Scan(&a.TenantID, &a.ID, &a.Key, &a.Name, &a.Role, &a.SystemPrompt, &rawsTools,
			&a.Model, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, fmt.Errorf("%w：Agent 不存在", ErrNotFound)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("更新 Agent 失败: %w", err)
	}
	a.Tools = current.Tools
	return a, nil
}

// DeleteAgent 删除 Agent；不存在返回 ErrNotFound。
func (s *Store) DeleteAgent(ctx context.Context, tenantID int64, key string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM agents WHERE tenant_id = $1 AND key = $2`, tenantID, strings.TrimSpace(key))
	if err != nil {
		return fmt.Errorf("删除 Agent 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：Agent 不存在", ErrNotFound)
	}
	return nil
}
