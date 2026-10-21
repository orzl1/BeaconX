package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件实现 B5-1 的 MCP 端点与工具清单存储（migration 0010）。
//
// 约定与其它 store 一致：
//   - 每个方法都显式接收 tenantID，所有 SQL 都带 tenant_id 条件；
//   - 「不存在」统一返回 ErrNotFound，「名称冲突」返回 ErrConflict，
//     「参数非法」返回 ErrInvalidInput，调用方用 errors.Is 判断；
//   - 返回的切片一律初始化为非 nil，HTTP 层才能稳定输出 [] 而不是 null；
//   - env/headers 按原值保存（刷新时要用），HTTP 层负责在响应里掩码；
//   - 刷新失败时只把脱敏摘要写进 last_error，绝不写入 env/headers 内容。

// mcpTransports 与 0010 的 CHECK 约束保持一致。
var mcpTransports = map[string]bool{
	"http":  true,
	"stdio": true,
}

// MCPServer 是一个 MCP 端点。
type MCPServer struct {
	TenantID        int64             `json:"tenant_id"`
	ID              int64             `json:"id"`
	Name            string            `json:"name"`
	Transport       string            `json:"transport"`
	URL             string            `json:"url"`
	Command         string            `json:"command"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env"`
	Headers         map[string]string `json:"headers"`
	Enabled         bool              `json:"enabled"`
	LastStatus      string            `json:"last_status"`
	LastError       string            `json:"last_error"`
	LastRefreshedAt *time.Time        `json:"last_refreshed_at,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// MCPTool 是一条发现到的工具。
type MCPTool struct {
	TenantID     int64          `json:"tenant_id"`
	ID           int64          `json:"id"`
	ServerID     int64          `json:"server_id"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	DiscoveredAt time.Time      `json:"discovered_at"`
}

// MCPServerInput 是创建端点的输入。
type MCPServerInput struct {
	Name      string
	Transport string
	URL       string
	Command   string
	Args      []string
	Env       map[string]string
	Headers   map[string]string
	Enabled   bool
}

// MCPServerUpdate 是端点局部更新输入：nil 指针表示不修改。
type MCPServerUpdate struct {
	Name    *string
	URL     *string
	Command *string
	Args    *[]string
	Env     *map[string]string
	Headers *map[string]string
	Enabled *bool
}

const mcpServerColumns = `tenant_id, id, name, transport, url, command, args, env, headers,
	enabled, last_status, last_error, last_refreshed_at, created_at, updated_at`

const mcpToolColumns = `tenant_id, id, server_id, name, description, input_schema, discovered_at`

// ValidateMCPServerInput 校验创建输入，并把 transport 相关的必填项收敛在一处。
func ValidateMCPServerInput(in MCPServerInput) (MCPServerInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Transport = strings.TrimSpace(in.Transport)
	in.URL = strings.TrimSpace(in.URL)
	in.Command = strings.TrimSpace(in.Command)
	if in.Name == "" {
		return in, fmt.Errorf("%w：端点名称不能为空", ErrInvalidInput)
	}
	if len(in.Name) > 200 {
		return in, fmt.Errorf("%w：端点名称不能超过 200 字节", ErrInvalidInput)
	}
	if !mcpTransports[in.Transport] {
		return in, fmt.Errorf("%w：transport 必须是 http 或 stdio", ErrInvalidInput)
	}
	switch in.Transport {
	case "http":
		if in.URL == "" {
			return in, fmt.Errorf("%w：http 传输必须提供 url", ErrInvalidInput)
		}
		parsed, err := url.Parse(in.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return in, fmt.Errorf("%w：url 必须是合法的 http/https 地址", ErrInvalidInput)
		}
	case "stdio":
		if in.Command == "" {
			return in, fmt.Errorf("%w：stdio 传输必须提供 command", ErrInvalidInput)
		}
	}
	if len(in.Args) > 100 {
		return in, fmt.Errorf("%w：args 最多 100 项", ErrInvalidInput)
	}
	if len(in.Env) > 100 || len(in.Headers) > 100 {
		return in, fmt.Errorf("%w：env/headers 最多各 100 项", ErrInvalidInput)
	}
	if in.Args == nil {
		in.Args = []string{}
	}
	if in.Env == nil {
		in.Env = map[string]string{}
	}
	if in.Headers == nil {
		in.Headers = map[string]string{}
	}
	return in, nil
}

func scanMCPServer(row pgx.Row) (MCPServer, error) {
	var (
		server      MCPServer
		argsRaw     []byte
		envRaw      []byte
		headersRaw  []byte
		refreshedAt *time.Time
	)
	err := row.Scan(&server.TenantID, &server.ID, &server.Name, &server.Transport, &server.URL,
		&server.Command, &argsRaw, &envRaw, &headersRaw, &server.Enabled,
		&server.LastStatus, &server.LastError, &refreshedAt, &server.CreatedAt, &server.UpdatedAt)
	if err != nil {
		return MCPServer{}, err
	}
	server.LastRefreshedAt = refreshedAt
	server.Args = []string{}
	if len(argsRaw) > 0 {
		_ = json.Unmarshal(argsRaw, &server.Args)
	}
	server.Env = map[string]string{}
	if len(envRaw) > 0 {
		_ = json.Unmarshal(envRaw, &server.Env)
	}
	server.Headers = map[string]string{}
	if len(headersRaw) > 0 {
		_ = json.Unmarshal(headersRaw, &server.Headers)
	}
	if server.Args == nil {
		server.Args = []string{}
	}
	if server.Env == nil {
		server.Env = map[string]string{}
	}
	if server.Headers == nil {
		server.Headers = map[string]string{}
	}
	return server, nil
}

func scanMCPTool(row pgx.Row) (MCPTool, error) {
	var (
		tool   MCPTool
		schema []byte
	)
	err := row.Scan(&tool.TenantID, &tool.ID, &tool.ServerID, &tool.Name,
		&tool.Description, &schema, &tool.DiscoveredAt)
	if err != nil {
		return MCPTool{}, err
	}
	tool.InputSchema = map[string]any{}
	if len(schema) > 0 {
		_ = json.Unmarshal(schema, &tool.InputSchema)
	}
	if tool.InputSchema == nil {
		tool.InputSchema = map[string]any{}
	}
	return tool, nil
}

// ListMCPServers 列出本租户全部端点（按 id 升序，保证 UI 顺序稳定）。
func (s *Store) ListMCPServers(ctx context.Context, tenantID int64) ([]MCPServer, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	const q = `SELECT ` + mcpServerColumns + ` FROM mcp_servers
		WHERE tenant_id = $1 ORDER BY id`
	rows, err := s.db.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("查询 MCP 端点失败：%w", err)
	}
	defer rows.Close()
	out := make([]MCPServer, 0, 8)
	for rows.Next() {
		server, err := scanMCPServer(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 MCP 端点失败：%w", err)
		}
		out = append(out, server)
	}
	return out, rows.Err()
}

// CreateMCPServer 创建端点；同租户内名称重复返回 ErrConflict。
func (s *Store) CreateMCPServer(ctx context.Context, tenantID int64, in MCPServerInput) (MCPServer, error) {
	if err := checkTenant(tenantID); err != nil {
		return MCPServer{}, err
	}
	validated, err := ValidateMCPServerInput(in)
	if err != nil {
		return MCPServer{}, err
	}
	argsRaw, err := json.Marshal(validated.Args)
	if err != nil {
		return MCPServer{}, fmt.Errorf("编码 args 失败：%w", err)
	}
	envRaw, err := json.Marshal(validated.Env)
	if err != nil {
		return MCPServer{}, fmt.Errorf("编码 env 失败：%w", err)
	}
	headersRaw, err := json.Marshal(validated.Headers)
	if err != nil {
		return MCPServer{}, fmt.Errorf("编码 headers 失败：%w", err)
	}
	const q = `INSERT INTO mcp_servers
		(tenant_id, name, transport, url, command, args, env, headers, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING ` + mcpServerColumns
	row := s.db.pool.QueryRow(ctx, q, tenantID, validated.Name, validated.Transport,
		validated.URL, validated.Command, argsRaw, envRaw, headersRaw, validated.Enabled)
	server, err := scanMCPServer(row)
	if isUniqueViolation(err) {
		return MCPServer{}, fmt.Errorf("%w：端点名称 %q 已存在", ErrConflict, validated.Name)
	}
	if err != nil {
		return MCPServer{}, fmt.Errorf("创建 MCP 端点失败：%w", err)
	}
	return server, nil
}

// GetMCPServer 读取端点详情。
func (s *Store) GetMCPServer(ctx context.Context, tenantID, id int64) (MCPServer, error) {
	if err := checkTenant(tenantID); err != nil {
		return MCPServer{}, err
	}
	const q = `SELECT ` + mcpServerColumns + ` FROM mcp_servers
		WHERE tenant_id = $1 AND id = $2`
	server, err := scanMCPServer(s.db.pool.QueryRow(ctx, q, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPServer{}, fmt.Errorf("%w：MCP 端点 %d 不存在", ErrNotFound, id)
	}
	if err != nil {
		return MCPServer{}, fmt.Errorf("读取 MCP 端点失败：%w", err)
	}
	return server, nil
}

// UpdateMCPServer 局部更新端点；transport 不可改（换传输方式等于换端点）。
func (s *Store) UpdateMCPServer(ctx context.Context, tenantID, id int64, upd MCPServerUpdate) (MCPServer, error) {
	if err := checkTenant(tenantID); err != nil {
		return MCPServer{}, err
	}
	current, err := s.GetMCPServer(ctx, tenantID, id)
	if err != nil {
		return MCPServer{}, err
	}
	// 先合并成完整输入再过一遍校验，避免把 http 改成没有 url 的非法状态。
	next := MCPServerInput{
		Name:      current.Name,
		Transport: current.Transport,
		URL:       current.URL,
		Command:   current.Command,
		Args:      current.Args,
		Env:       current.Env,
		Headers:   current.Headers,
		Enabled:   current.Enabled,
	}
	if upd.Name != nil {
		next.Name = *upd.Name
	}
	if upd.URL != nil {
		next.URL = *upd.URL
	}
	if upd.Command != nil {
		next.Command = *upd.Command
	}
	if upd.Args != nil {
		next.Args = *upd.Args
	}
	if upd.Env != nil {
		next.Env = *upd.Env
	}
	if upd.Headers != nil {
		next.Headers = *upd.Headers
	}
	if upd.Enabled != nil {
		next.Enabled = *upd.Enabled
	}
	if len(next.Name) == 0 {
		return MCPServer{}, fmt.Errorf("%w：端点名称不能为空", ErrInvalidInput)
	}
	validated, err := ValidateMCPServerInput(next)
	if err != nil {
		return MCPServer{}, err
	}
	argsRaw, _ := json.Marshal(validated.Args)
	envRaw, _ := json.Marshal(validated.Env)
	headersRaw, _ := json.Marshal(validated.Headers)
	const q = `UPDATE mcp_servers SET
			name = $3, url = $4, command = $5, args = $6, env = $7, headers = $8, enabled = $9
		WHERE tenant_id = $1 AND id = $2
		RETURNING ` + mcpServerColumns
	server, err := scanMCPServer(s.db.pool.QueryRow(ctx, q, tenantID, id,
		validated.Name, validated.URL, validated.Command, argsRaw, envRaw, headersRaw, validated.Enabled))
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPServer{}, fmt.Errorf("%w：MCP 端点 %d 不存在", ErrNotFound, id)
	}
	if isUniqueViolation(err) {
		return MCPServer{}, fmt.Errorf("%w：端点名称 %q 已存在", ErrConflict, validated.Name)
	}
	if err != nil {
		return MCPServer{}, fmt.Errorf("更新 MCP 端点失败：%w", err)
	}
	return server, nil
}

// DeleteMCPServer 删除端点（工具清单级联删除）。
func (s *Store) DeleteMCPServer(ctx context.Context, tenantID, id int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM mcp_servers WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("删除 MCP 端点失败：%w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：MCP 端点 %d 不存在", ErrNotFound, id)
	}
	return nil
}

// ListMCPTools 列出工具；serverID=0 表示全部端点。
func (s *Store) ListMCPTools(ctx context.Context, tenantID, serverID int64) ([]MCPTool, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	var (
		rows pgx.Rows
		err  error
	)
	if serverID > 0 {
		rows, err = s.db.pool.Query(ctx,
			`SELECT `+mcpToolColumns+` FROM mcp_tools
			 WHERE tenant_id = $1 AND server_id = $2 ORDER BY server_id, name`, tenantID, serverID)
	} else {
		rows, err = s.db.pool.Query(ctx,
			`SELECT `+mcpToolColumns+` FROM mcp_tools
			 WHERE tenant_id = $1 ORDER BY server_id, name`, tenantID)
	}
	if err != nil {
		return nil, fmt.Errorf("查询 MCP 工具失败：%w", err)
	}
	defer rows.Close()
	out := make([]MCPTool, 0, 16)
	for rows.Next() {
		tool, err := scanMCPTool(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 MCP 工具失败：%w", err)
		}
		out = append(out, tool)
	}
	return out, rows.Err()
}

// ReplaceMCPTools 用一次成功发现的结果整体替换端点工具，并把状态置为 ok。
//
// 事务边界：删除旧行 + 插入新行 + 更新端点状态必须原子，否则刷新失败时会留下
// 半套工具清单。空清单也允许（端点没有暴露任何工具是合法状态）。
func (s *Store) ReplaceMCPTools(ctx context.Context, tenantID, serverID int64, tools []MCPTool) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if serverID <= 0 {
		return fmt.Errorf("%w：端点标识不合法", ErrInvalidInput)
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 端点必须存在且属于本租户；同时锁住该行，避免并发刷新交错。
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM mcp_servers WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, serverID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w：MCP 端点 %d 不存在", ErrNotFound, serverID)
		}
		return fmt.Errorf("校验 MCP 端点失败：%w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM mcp_tools WHERE tenant_id = $1 AND server_id = $2`, tenantID, serverID); err != nil {
		return fmt.Errorf("清理旧工具清单失败：%w", err)
	}
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{}
		}
		schemaRaw, err := json.Marshal(schema)
		if err != nil {
			return fmt.Errorf("编码工具 schema 失败：%w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_tools
			(tenant_id, server_id, name, description, input_schema)
			VALUES ($1, $2, $3, $4, $5)`,
			tenantID, serverID, name, tool.Description, schemaRaw); err != nil {
			return fmt.Errorf("写入工具 %q 失败：%w", name, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE mcp_servers SET
			last_status = 'ok', last_error = '', last_refreshed_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, serverID); err != nil {
		return fmt.Errorf("更新端点刷新状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交刷新结果失败：%w", err)
	}
	return nil
}

// MarkMCPRefreshFailed 记录一次失败的刷新（不覆盖上一次成功的工具清单）。
//
// message 由调用方保证已脱敏：本方法只做长度截断，不做内容清洗。
func (s *Store) MarkMCPRefreshFailed(ctx context.Context, tenantID, serverID int64, message string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	message = strings.TrimSpace(message)
	if len([]rune(message)) > 500 {
		message = string([]rune(message)[:500])
	}
	tag, err := s.db.pool.Exec(ctx, `UPDATE mcp_servers SET
			last_status = 'error', last_error = $3, last_refreshed_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, serverID, message)
	if err != nil {
		return fmt.Errorf("记录刷新失败状态失败：%w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：MCP 端点 %d 不存在", ErrNotFound, serverID)
	}
	return nil
}
