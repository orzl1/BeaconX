package pg

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sync（外部数据源同步，B5）的存储层。
//
// 三张表的分工见 migrations/0015_sync.sql：
//   - sync_sources 保存数据源配置；
//   - sync_projects 保存同步产出的外部项目（按 source + external_id 幂等）；
//   - sync_runs 是只追加的运行记录，状态机由本文件的条件 UPDATE 保证。
//
// 约定与其它 Store 方法一致：每个方法显式接收 tenantID，所有 SQL 都带
// tenant_id 条件；「不存在」返回 ErrNotFound，「唯一键冲突」返回 ErrConflict，
// 「入参非法」返回 ErrInvalidInput，调用方用 errors.Is 判断。

// 数据源类型：manual 不联网（项目由 API 写入）；http_json 通过 GET 拉取 JSON。
const (
	SyncSourceManual   = "manual"
	SyncSourceHTTPJSON = "http_json"
)

// 运行状态。
const (
	SyncRunPending   = "pending"
	SyncRunRunning   = "running"
	SyncRunSucceeded = "succeeded"
	SyncRunFailed    = "failed"
)

// syncRunTerminalStatuses 是终态集合：终态运行不再接受状态迁移。
var syncRunTerminalStatuses = map[string]bool{
	SyncRunSucceeded: true,
	SyncRunFailed:    true,
}

// SyncSource 是一个外部数据源。
type SyncSource struct {
	TenantID  int64          `json:"tenant_id"`
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	Config    map[string]any `json:"config"`
	Enabled   bool           `json:"enabled"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// SyncSourceInput 是创建数据源的输入。
type SyncSourceInput struct {
	Name    string
	Type    string
	Config  map[string]any
	Enabled *bool
}

// SyncSourcePatch 是数据源的局部更新输入：nil 字段保持不变。
type SyncSourcePatch struct {
	Name    *string
	Type    *string
	Config  map[string]any
	Enabled *bool
}

// SyncProject 是一条外部项目记录。
type SyncProject struct {
	TenantID   int64          `json:"tenant_id"`
	ID         int64          `json:"id"`
	SourceID   int64          `json:"source_id"`
	ExternalID string         `json:"external_id"`
	Name       string         `json:"name"`
	Data       map[string]any `json:"data"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// SyncProjectInput 是写入/更新外部项目的输入。
type SyncProjectInput struct {
	SourceID   int64
	ExternalID string
	Name       string
	Data       map[string]any
}

// SyncRun 是一次同步运行记录。
type SyncRun struct {
	TenantID   int64          `json:"tenant_id"`
	ID         int64          `json:"id"`
	SourceID   int64          `json:"source_id"`
	ProjectID  *int64         `json:"project_id,omitempty"`
	Status     string         `json:"status"`
	Stats      map[string]any `json:"stats"`
	Error      string         `json:"error"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// SyncRunFilter 是运行记录的过滤条件。
type SyncRunFilter struct {
	SourceID int64
	Status   string
	Limit    int
}

// validSyncSourceType 报告 type 是否为支持的数据源类型。
func validSyncSourceType(typ string) bool {
	switch typ {
	case SyncSourceManual, SyncSourceHTTPJSON:
		return true
	}
	return false
}

// validSyncRunStatus 报告 status 是否为已知运行状态。
func validSyncRunStatus(status string) bool {
	switch status {
	case SyncRunPending, SyncRunRunning, SyncRunSucceeded, SyncRunFailed:
		return true
	}
	return false
}

// validateSyncSourceConfig 校验数据源配置。
//
// 只做「不联网就能判断」的校验：config 必须是对象；http_json 必须有合法的
// http/https URL，headers 必须是字符串到字符串的映射。私网/回环地址的拦截
// 放在执行期（syncdata 包），因为同一个 URL 是否可达取决于部署环境与
// allow_private 配置，保存时不应该把它写死。
func validateSyncSourceConfig(typ string, config map[string]any) error {
	if len(config) == 0 {
		if typ == SyncSourceHTTPJSON {
			return fmt.Errorf("%w：http_json 数据源必须提供 config.url", ErrInvalidInput)
		}
		return nil
	}
	if typ != SyncSourceHTTPJSON {
		// manual 数据源允许携带备注类配置，但不解析。
		return nil
	}
	rawURL, ok := config["url"].(string)
	if !ok || strings.TrimSpace(rawURL) == "" {
		return fmt.Errorf("%w：http_json 数据源必须提供 config.url", ErrInvalidInput)
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("%w：config.url 不是合法 URL", ErrInvalidInput)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%w：config.url 只支持 http/https", ErrInvalidInput)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w：config.url 缺少主机", ErrInvalidInput)
	}
	if headers, exists := config["headers"]; exists && headers != nil {
		raw, ok := headers.(map[string]any)
		if !ok {
			return fmt.Errorf("%w：config.headers 必须是字符串映射", ErrInvalidInput)
		}
		for key, value := range raw {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%w：config.headers.%s 必须是字符串", ErrInvalidInput, key)
			}
		}
	}
	return nil
}

// CreateSyncSource 创建数据源；同租户内 name 重复返回 ErrConflict。
func (s *Store) CreateSyncSource(ctx context.Context, tenantID int64, in SyncSourceInput) (SyncSource, error) {
	if err := checkTenant(tenantID); err != nil {
		return SyncSource{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return SyncSource{}, fmt.Errorf("%w：数据源名称不能为空", ErrInvalidInput)
	}
	if len(name) > 200 {
		return SyncSource{}, fmt.Errorf("%w：数据源名称长度不能超过 200 字节", ErrInvalidInput)
	}
	typ := strings.TrimSpace(in.Type)
	if !validSyncSourceType(typ) {
		return SyncSource{}, fmt.Errorf("%w：非法数据源类型 %q（只允许 manual/http_json）", ErrInvalidInput, typ)
	}
	config := in.Config
	if config == nil {
		config = map[string]any{}
	}
	if err := validateSyncSourceConfig(typ, config); err != nil {
		return SyncSource{}, err
	}
	raw, err := marshalJSON(config)
	if err != nil {
		return SyncSource{}, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	var (
		out  SyncSource
		conf []byte
	)
	err = s.db.pool.QueryRow(ctx, `
		INSERT INTO sync_sources (tenant_id, name, type, config, enabled)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		RETURNING tenant_id, id, name, type, config, enabled, created_at, updated_at`,
		tenantID, name, typ, string(raw), enabled).
		Scan(&out.TenantID, &out.ID, &out.Name, &out.Type, &conf, &out.Enabled, &out.CreatedAt, &out.UpdatedAt)
	if isUniqueViolation(err) {
		return SyncSource{}, fmt.Errorf("%w：数据源名称 %q 已存在", ErrConflict, name)
	}
	if err != nil {
		return SyncSource{}, fmt.Errorf("创建数据源失败: %w", err)
	}
	if out.Config, err = decodeJSON(conf); err != nil {
		return SyncSource{}, err
	}
	return out, nil
}

// GetSyncSource 按 ID 读取数据源（跨租户同样返回 ErrNotFound）。
func (s *Store) GetSyncSource(ctx context.Context, tenantID, sourceID int64) (SyncSource, error) {
	if err := checkTenant(tenantID); err != nil {
		return SyncSource{}, err
	}
	if sourceID <= 0 {
		return SyncSource{}, fmt.Errorf("%w：数据源标识必须为正数", ErrInvalidInput)
	}
	var (
		out  SyncSource
		conf []byte
	)
	err := s.db.pool.QueryRow(ctx, `
		SELECT tenant_id, id, name, type, config, enabled, created_at, updated_at
		FROM sync_sources WHERE tenant_id = $1 AND id = $2`,
		tenantID, sourceID).
		Scan(&out.TenantID, &out.ID, &out.Name, &out.Type, &conf, &out.Enabled, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncSource{}, fmt.Errorf("%w：数据源 %d", ErrNotFound, sourceID)
	}
	if err != nil {
		return SyncSource{}, fmt.Errorf("读取数据源失败: %w", err)
	}
	if out.Config, err = decodeJSON(conf); err != nil {
		return SyncSource{}, err
	}
	return out, nil
}

// ListSyncSources 列出数据源（id 升序）。
func (s *Store) ListSyncSources(ctx context.Context, tenantID int64, limit int) ([]SyncSource, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit = normalizeLimit(limit, defaultListLimit, maxListLimit)
	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, name, type, config, enabled, created_at, updated_at
		FROM sync_sources WHERE tenant_id = $1 ORDER BY id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出数据源失败: %w", err)
	}
	defer rows.Close()

	out := make([]SyncSource, 0, limit)
	for rows.Next() {
		var (
			item SyncSource
			conf []byte
		)
		if err := rows.Scan(&item.TenantID, &item.ID, &item.Name, &item.Type, &conf,
			&item.Enabled, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析数据源失败: %w", err)
		}
		if item.Config, err = decodeJSON(conf); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历数据源失败: %w", err)
	}
	return out, nil
}

// UpdateSyncSource 局部更新数据源：nil 字段保持不变。
func (s *Store) UpdateSyncSource(ctx context.Context, tenantID, sourceID int64, in SyncSourcePatch) (SyncSource, error) {
	if err := checkTenant(tenantID); err != nil {
		return SyncSource{}, err
	}
	current, err := s.GetSyncSource(ctx, tenantID, sourceID)
	if err != nil {
		return SyncSource{}, err
	}

	sets := make([]string, 0, 4)
	args := []any{tenantID, sourceID}

	typ := current.Type
	if in.Type != nil {
		typ = strings.TrimSpace(*in.Type)
		if !validSyncSourceType(typ) {
			return SyncSource{}, fmt.Errorf("%w：非法数据源类型 %q（只允许 manual/http_json）", ErrInvalidInput, typ)
		}
		args = append(args, typ)
		sets = append(sets, fmt.Sprintf("type = $%d", len(args)))
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return SyncSource{}, fmt.Errorf("%w：数据源名称不能为空", ErrInvalidInput)
		}
		if len(name) > 200 {
			return SyncSource{}, fmt.Errorf("%w：数据源名称长度不能超过 200 字节", ErrInvalidInput)
		}
		args = append(args, name)
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	config := current.Config
	if in.Config != nil {
		config = in.Config
	}
	// 类型或配置变更后都要重新校验，避免 PATCH 把 http_json 改成没有 url 的配置。
	if in.Type != nil || in.Config != nil {
		if err := validateSyncSourceConfig(typ, config); err != nil {
			return SyncSource{}, err
		}
	}
	if in.Config != nil {
		raw, err := marshalJSON(in.Config)
		if err != nil {
			return SyncSource{}, err
		}
		args = append(args, string(raw))
		sets = append(sets, fmt.Sprintf("config = $%d::jsonb", len(args)))
	}
	if in.Enabled != nil {
		args = append(args, *in.Enabled)
		sets = append(sets, fmt.Sprintf("enabled = $%d", len(args)))
	}
	if len(sets) == 0 {
		return current, nil
	}

	var (
		out  SyncSource
		conf []byte
	)
	err = s.db.pool.QueryRow(ctx, `
		UPDATE sync_sources SET `+strings.Join(sets, ", ")+`
		WHERE tenant_id = $1 AND id = $2
		RETURNING tenant_id, id, name, type, config, enabled, created_at, updated_at`,
		args...).
		Scan(&out.TenantID, &out.ID, &out.Name, &out.Type, &conf, &out.Enabled, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncSource{}, fmt.Errorf("%w：数据源 %d", ErrNotFound, sourceID)
	}
	if isUniqueViolation(err) {
		return SyncSource{}, fmt.Errorf("%w：数据源名称已存在", ErrConflict)
	}
	if err != nil {
		return SyncSource{}, fmt.Errorf("更新数据源失败: %w", err)
	}
	if out.Config, err = decodeJSON(conf); err != nil {
		return SyncSource{}, err
	}
	return out, nil
}

// DeleteSyncSource 删除数据源；其项目与运行记录随外键级联删除。
func (s *Store) DeleteSyncSource(ctx context.Context, tenantID, sourceID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM sync_sources WHERE tenant_id = $1 AND id = $2`, tenantID, sourceID)
	if err != nil {
		return fmt.Errorf("删除数据源失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：数据源 %d", ErrNotFound, sourceID)
	}
	return nil
}

// validateSyncProjectInput 校验外部项目输入，并返回 trim 后的 external_id 与默认 name。
func validateSyncProjectInput(in SyncProjectInput) (externalID, name string, err error) {
	externalID = strings.TrimSpace(in.ExternalID)
	if externalID == "" {
		return "", "", fmt.Errorf("%w：external_id 不能为空", ErrInvalidInput)
	}
	if len(externalID) > 200 {
		return "", "", fmt.Errorf("%w：external_id 长度不能超过 200 字节", ErrInvalidInput)
	}
	name = strings.TrimSpace(in.Name)
	if name == "" {
		name = externalID
	}
	return externalID, name, nil
}

// UpsertSyncProject 幂等写入外部项目：(tenant, source, external_id) 唯一。
//
// 同步执行器导入时用它（重复同步只更新 name/data），因此重复不报错。
func (s *Store) UpsertSyncProject(ctx context.Context, tenantID int64, in SyncProjectInput) (SyncProject, error) {
	if err := checkTenant(tenantID); err != nil {
		return SyncProject{}, err
	}
	if in.SourceID <= 0 {
		return SyncProject{}, fmt.Errorf("%w：数据源标识必须为正数", ErrInvalidInput)
	}
	externalID, name, err := validateSyncProjectInput(in)
	if err != nil {
		return SyncProject{}, err
	}
	data := in.Data
	if data == nil {
		data = map[string]any{}
	}
	raw, err := marshalJSON(data)
	if err != nil {
		return SyncProject{}, err
	}
	// 数据源必须属于当前租户：外键只能保证存在，跨租户要显式翻译成 404。
	if _, err := s.GetSyncSource(ctx, tenantID, in.SourceID); err != nil {
		return SyncProject{}, err
	}

	var (
		out  SyncProject
		body []byte
	)
	err = s.db.pool.QueryRow(ctx, `
		INSERT INTO sync_projects (tenant_id, source_id, external_id, name, data)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		ON CONFLICT (tenant_id, source_id, external_id) DO UPDATE
		SET name = EXCLUDED.name, data = EXCLUDED.data
		RETURNING tenant_id, id, source_id, external_id, name, data, created_at, updated_at`,
		tenantID, in.SourceID, externalID, name, string(raw)).
		Scan(&out.TenantID, &out.ID, &out.SourceID, &out.ExternalID, &out.Name, &body,
			&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return SyncProject{}, fmt.Errorf("写入外部项目失败: %w", err)
	}
	if out.Data, err = decodeJSON(body); err != nil {
		return SyncProject{}, err
	}
	return out, nil
}

// CreateSyncProject 创建外部项目：external_id 已存在返回 ErrConflict。
//
// 与 UpsertSyncProject 分开：HTTP 的 POST /sync/projects 是「用户关联一个外部
// 项目」，重复应当报 409；而同步执行器导入时用 upsert 静默合并。
func (s *Store) CreateSyncProject(ctx context.Context, tenantID int64, in SyncProjectInput) (SyncProject, error) {
	if err := checkTenant(tenantID); err != nil {
		return SyncProject{}, err
	}
	externalID, _, err := validateSyncProjectInput(in)
	if err != nil {
		return SyncProject{}, err
	}
	if in.SourceID <= 0 {
		return SyncProject{}, fmt.Errorf("%w：数据源标识必须为正数", ErrInvalidInput)
	}
	if _, err := s.GetSyncSource(ctx, tenantID, in.SourceID); err != nil {
		return SyncProject{}, err
	}
	var exists bool
	if err := s.db.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM sync_projects
		                WHERE tenant_id = $1 AND source_id = $2 AND external_id = $3)`,
		tenantID, in.SourceID, externalID).Scan(&exists); err != nil {
		return SyncProject{}, fmt.Errorf("校验外部项目失败: %w", err)
	}
	if exists {
		return SyncProject{}, fmt.Errorf("%w：数据源下已存在 external_id %q 的项目", ErrConflict, externalID)
	}
	return s.UpsertSyncProject(ctx, tenantID, in)
}

// GetSyncProject 按 ID 读取外部项目。
func (s *Store) GetSyncProject(ctx context.Context, tenantID, projectID int64) (SyncProject, error) {
	if err := checkTenant(tenantID); err != nil {
		return SyncProject{}, err
	}
	var (
		out  SyncProject
		body []byte
	)
	err := s.db.pool.QueryRow(ctx, `
		SELECT tenant_id, id, source_id, external_id, name, data, created_at, updated_at
		FROM sync_projects WHERE tenant_id = $1 AND id = $2`, tenantID, projectID).
		Scan(&out.TenantID, &out.ID, &out.SourceID, &out.ExternalID, &out.Name, &body,
			&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncProject{}, fmt.Errorf("%w：外部项目 %d", ErrNotFound, projectID)
	}
	if err != nil {
		return SyncProject{}, fmt.Errorf("读取外部项目失败: %w", err)
	}
	if out.Data, err = decodeJSON(body); err != nil {
		return SyncProject{}, err
	}
	return out, nil
}

// ListSyncProjects 列出外部项目；sourceID>0 时按数据源过滤。
func (s *Store) ListSyncProjects(ctx context.Context, tenantID, sourceID int64, limit int) ([]SyncProject, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if sourceID < 0 {
		return nil, fmt.Errorf("%w：source_id 不能为负数", ErrInvalidInput)
	}
	limit = normalizeLimit(limit, defaultListLimit, maxListLimit)

	args := []any{tenantID}
	conds := []string{"tenant_id = $1"}
	if sourceID > 0 {
		args = append(args, sourceID)
		conds = append(conds, fmt.Sprintf("source_id = $%d", len(args)))
	}
	args = append(args, limit)

	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, source_id, external_id, name, data, created_at, updated_at
		FROM sync_projects WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY id LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("列出外部项目失败: %w", err)
	}
	defer rows.Close()

	out := make([]SyncProject, 0, limit)
	for rows.Next() {
		var (
			item SyncProject
			body []byte
		)
		if err := rows.Scan(&item.TenantID, &item.ID, &item.SourceID, &item.ExternalID,
			&item.Name, &body, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析外部项目失败: %w", err)
		}
		if item.Data, err = decodeJSON(body); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历外部项目失败: %w", err)
	}
	return out, nil
}

// CountSyncProjects 统计数据源下的外部项目数（manual 同步的统计口径）。
func (s *Store) CountSyncProjects(ctx context.Context, tenantID, sourceID int64) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	var count int64
	if err := s.db.pool.QueryRow(ctx,
		`SELECT count(*) FROM sync_projects WHERE tenant_id = $1 AND source_id = $2`,
		tenantID, sourceID).Scan(&count); err != nil {
		return 0, fmt.Errorf("统计外部项目失败: %w", err)
	}
	return count, nil
}

// CreateSyncRun 创建一条「正在运行」的同步记录。
//
// 直接以 running 落库（而不是先 pending 再改）：HTTP 的 POST /sync/run 返回时
// 执行已经开始，pending 是一个调用方永远观察不到、只会让轮询逻辑多一种中间态
// 的状态。如果执行器启动失败，会立刻把它置为 failed。
func (s *Store) CreateSyncRun(ctx context.Context, tenantID, sourceID int64, projectID *int64) (SyncRun, error) {
	if err := checkTenant(tenantID); err != nil {
		return SyncRun{}, err
	}
	if sourceID <= 0 {
		return SyncRun{}, fmt.Errorf("%w：数据源标识必须为正数", ErrInvalidInput)
	}
	if _, err := s.GetSyncSource(ctx, tenantID, sourceID); err != nil {
		return SyncRun{}, err
	}
	if projectID != nil {
		project, err := s.GetSyncProject(ctx, tenantID, *projectID)
		if err != nil {
			return SyncRun{}, err
		}
		if project.SourceID != sourceID {
			return SyncRun{}, fmt.Errorf("%w：项目 %d 不属于数据源 %d", ErrInvalidInput, *projectID, sourceID)
		}
	}

	var (
		out   SyncRun
		stats []byte
	)
	err := s.db.pool.QueryRow(ctx, `
		INSERT INTO sync_runs (tenant_id, source_id, project_id, status, started_at)
		VALUES ($1, $2, $3, 'running', now())
		RETURNING tenant_id, id, source_id, project_id, status, stats, error,
		          started_at, finished_at, created_at`,
		tenantID, sourceID, projectID).
		Scan(&out.TenantID, &out.ID, &out.SourceID, &out.ProjectID, &out.Status, &stats, &out.Error,
			&out.StartedAt, &out.FinishedAt, &out.CreatedAt)
	if err != nil {
		return SyncRun{}, fmt.Errorf("创建同步运行失败: %w", err)
	}
	if out.Stats, err = decodeJSON(stats); err != nil {
		return SyncRun{}, err
	}
	return out, nil
}

// FinishSyncRun 结束一条运行记录：只允许从 running 迁移到终态。
//
// 用条件 UPDATE 而不是「先查再写」：并发或重复调用时，第二次会拿到
// ErrConflict，而不是覆盖掉第一次的统计。
func (s *Store) FinishSyncRun(ctx context.Context, tenantID, runID int64, status string, stats map[string]any, errText string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if !syncRunTerminalStatuses[status] {
		return fmt.Errorf("%w：结束状态只能是 succeeded/failed", ErrInvalidInput)
	}
	if len(errText) > 2000 {
		// 错误信息只用于排障，截断避免把整段上游响应灌进运行记录。
		errText = errText[:2000]
	}
	if stats == nil {
		stats = map[string]any{}
	}
	raw, err := marshalJSON(stats)
	if err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx, `
		UPDATE sync_runs
		SET status = $3, stats = $4::jsonb, error = $5, finished_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status = 'running'`,
		tenantID, runID, status, string(raw), errText)
	if err != nil {
		return fmt.Errorf("结束同步运行失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：运行 %d 不存在或已结束", ErrConflict, runID)
	}
	return nil
}

// GetSyncRun 按 ID 读取运行记录。
func (s *Store) GetSyncRun(ctx context.Context, tenantID, runID int64) (SyncRun, error) {
	if err := checkTenant(tenantID); err != nil {
		return SyncRun{}, err
	}
	var (
		out   SyncRun
		stats []byte
	)
	err := s.db.pool.QueryRow(ctx, `
		SELECT tenant_id, id, source_id, project_id, status, stats, error,
		       started_at, finished_at, created_at
		FROM sync_runs WHERE tenant_id = $1 AND id = $2`, tenantID, runID).
		Scan(&out.TenantID, &out.ID, &out.SourceID, &out.ProjectID, &out.Status, &stats, &out.Error,
			&out.StartedAt, &out.FinishedAt, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncRun{}, fmt.Errorf("%w：同步运行 %d", ErrNotFound, runID)
	}
	if err != nil {
		return SyncRun{}, fmt.Errorf("读取同步运行失败: %w", err)
	}
	if out.Stats, err = decodeJSON(stats); err != nil {
		return SyncRun{}, err
	}
	return out, nil
}

// ListSyncRuns 列出运行记录（id 倒序），可按数据源与状态过滤。
func (s *Store) ListSyncRuns(ctx context.Context, tenantID int64, f SyncRunFilter) ([]SyncRun, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	status := strings.TrimSpace(f.Status)
	if status != "" && !validSyncRunStatus(status) {
		return nil, fmt.Errorf("%w：未知的同步状态 %q", ErrInvalidInput, status)
	}
	limit := normalizeLimit(f.Limit, defaultListLimit, maxListLimit)

	args := []any{tenantID}
	conds := []string{"tenant_id = $1"}
	if f.SourceID > 0 {
		args = append(args, f.SourceID)
		conds = append(conds, fmt.Sprintf("source_id = $%d", len(args)))
	}
	if status != "" {
		args = append(args, status)
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	args = append(args, limit)

	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, source_id, project_id, status, stats, error,
		       started_at, finished_at, created_at
		FROM sync_runs WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("列出同步运行失败: %w", err)
	}
	defer rows.Close()

	out := make([]SyncRun, 0, limit)
	for rows.Next() {
		var (
			item  SyncRun
			stats []byte
		)
		if err := rows.Scan(&item.TenantID, &item.ID, &item.SourceID, &item.ProjectID,
			&item.Status, &stats, &item.Error, &item.StartedAt, &item.FinishedAt,
			&item.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析同步运行失败: %w", err)
		}
		if item.Stats, err = decodeJSON(stats); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历同步运行失败: %w", err)
	}
	return out, nil
}

// syncRunRecoveredError 是进程重启恢复写入的固定错误文案（便于运维检索）。
const syncRunRecoveredError = "进程重启恢复"

// RecoverStaleSyncRuns 把超时仍停在 running 的同步运行标记为 failed。
//
// 进程崩溃会留下 status='running' 且永不结束的记录；启动时用固定的 staleAfter
// 判定（默认 15 分钟）避免把刚刚启动、正常执行的运行误判。只按 started_at 判断，
// started_at 为 NULL 的历史脏数据保持原状（无法判断开始时间，交给人工处理）。
func (s *Store) RecoverStaleSyncRuns(ctx context.Context, tenantID int64, staleAfter time.Duration) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	if staleAfter <= 0 {
		return 0, fmt.Errorf("%w：恢复阈值必须大于 0", ErrInvalidInput)
	}
	tag, err := s.db.pool.Exec(ctx, `
		UPDATE sync_runs
		SET status = 'failed', error = $2, finished_at = now()
		WHERE tenant_id = $1 AND status = 'running'
		  AND started_at IS NOT NULL
		  AND started_at < now() - make_interval(secs => $3::double precision)`,
		tenantID, syncRunRecoveredError, staleAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("恢复中断的同步运行失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteOldSyncRuns 删除超过保留期的终态同步运行（succeeded/failed）。
//
// retention <= 0 表示不清理（保留全部历史）；只删 finished_at 明确早于阈值的行，
// 未结束（pending/running）与 finished_at 为空的行永不清理，避免误删运行中数据。
func (s *Store) DeleteOldSyncRuns(ctx context.Context, tenantID int64, retention time.Duration) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	if retention <= 0 {
		return 0, nil
	}
	days := retention / (24 * time.Hour)
	if days <= 0 {
		days = 1
	}
	tag, err := s.db.pool.Exec(ctx, `
		DELETE FROM sync_runs
		WHERE tenant_id = $1
		  AND status IN ('succeeded', 'failed')
		  AND finished_at IS NOT NULL
		  AND finished_at < now() - make_interval(days => $2::int)`,
		tenantID, int(days))
	if err != nil {
		return 0, fmt.Errorf("清理过期同步运行失败: %w", err)
	}
	return tag.RowsAffected(), nil
}
