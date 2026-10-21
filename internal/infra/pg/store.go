package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Store 是资产图与作战图的数据访问入口。
//
// 每个方法都显式接收 tenantID：v1 虽是单租户，但把所有查询都带上租户条件，
// 后续开启多租户时不需要逐个审计 SQL。
type Store struct {
	db *DB
}

// NewStore 基于连接池创建 Store。
func NewStore(db *DB) *Store {
	return &Store{db: db}
}

// checkTenant 校验租户标识。
func checkTenant(tenantID int64) error {
	if tenantID <= 0 {
		return ErrInvalidTenant
	}
	return nil
}

// marshalJSON 把 map 编码为 JSONB 文本；nil 视为空对象。
func marshalJSON(v map[string]any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("编码 JSON 属性失败: %w", err)
	}
	return raw, nil
}

// decodeJSON 把 JSONB 文本解码为 map；空值视为空对象。
func decodeJSON(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解码 JSON 属性失败: %w", err)
	}
	return out, nil
}

// ---------- 公司 ----------

// CreateCompany 创建公司；同租户内 slug 唯一，重复时返回 ErrConflict。
func (s *Store) CreateCompany(ctx context.Context, tenantID int64, name, slug string) (Company, error) {
	if err := checkTenant(tenantID); err != nil {
		return Company{}, err
	}
	name = strings.TrimSpace(name)
	slug = strings.TrimSpace(slug)
	if name == "" || slug == "" {
		return Company{}, fmt.Errorf("%w: 公司名称与 slug 不能为空", ErrInvalidInput)
	}
	if len(name) > 200 || len(slug) > 200 {
		return Company{}, fmt.Errorf("%w: 公司名称与 slug 长度不能超过 200 字节", ErrInvalidInput)
	}
	const q = `
		INSERT INTO companies (tenant_id, name, slug)
		VALUES ($1, $2, $3)
		RETURNING tenant_id, id, name, slug, attrs, created_at, updated_at`
	var c Company
	var attrs []byte
	err := s.db.pool.QueryRow(ctx, q, tenantID, name, slug).
		Scan(&c.TenantID, &c.ID, &c.Name, &c.Slug, &attrs, &c.CreatedAt, &c.UpdatedAt)
	if isUniqueViolation(err) {
		return Company{}, fmt.Errorf("%w: slug %q 已被其他企业使用", ErrConflict, slug)
	}
	if err != nil {
		return Company{}, fmt.Errorf("创建公司失败: %w", err)
	}
	if c.Attrs, err = decodeJSON(attrs); err != nil {
		return Company{}, err
	}
	return c, nil
}

// GetCompany 按 ID 读取公司。
func (s *Store) GetCompany(ctx context.Context, tenantID, companyID int64) (Company, error) {
	if err := checkTenant(tenantID); err != nil {
		return Company{}, err
	}
	const q = `
		SELECT tenant_id, id, name, slug, attrs, created_at, updated_at
		FROM companies WHERE tenant_id = $1 AND id = $2`
	var c Company
	var attrs []byte
	err := s.db.pool.QueryRow(ctx, q, tenantID, companyID).
		Scan(&c.TenantID, &c.ID, &c.Name, &c.Slug, &attrs, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Company{}, fmt.Errorf("读取公司失败: %w", err)
	}
	if c.Attrs, err = decodeJSON(attrs); err != nil {
		return Company{}, err
	}
	return c, nil
}

// ---------- 任务 ----------

// CreateMission 创建任务。
func (s *Store) CreateMission(ctx context.Context, tenantID int64, in MissionInput) (Mission, error) {
	if err := checkTenant(tenantID); err != nil {
		return Mission{}, err
	}
	if in.Name == "" {
		return Mission{}, fmt.Errorf("%w: 任务名称不能为空", ErrInvalidInput)
	}
	const q = `
		INSERT INTO missions (tenant_id, name, goal, description, status, queued, queued_at)
		VALUES ($1, $2, $3, $4, 'queued', true, now())
		RETURNING tenant_id, id, name, goal, description, status, queued, queued_at,
		          started_at, finished_at, deadline_at, reason,
		          max_tokens, spent_tokens, max_steps, steps_used, created_at, updated_at`
	var m Mission
	err := s.db.pool.QueryRow(ctx, q, tenantID, in.Name, in.Goal, in.Description).
		Scan(&m.TenantID, &m.ID, &m.Name, &m.Goal, &m.Description, &m.Status, &m.Queued,
			&m.QueuedAt, &m.StartedAt, &m.FinishedAt, &m.DeadlineAt, &m.Reason,
			&m.MaxTokens, &m.SpentTokens, &m.MaxSteps, &m.StepsUsed, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return Mission{}, fmt.Errorf("创建任务失败: %w", err)
	}
	return m, nil
}

// GetMission 按 ID 读取任务。
func (s *Store) GetMission(ctx context.Context, tenantID, missionID int64) (Mission, error) {
	if err := checkTenant(tenantID); err != nil {
		return Mission{}, err
	}
	const q = `
		SELECT ` + missionColumns + `
		FROM missions WHERE tenant_id = $1 AND id = $2`
	var m Mission
	err := s.db.pool.QueryRow(ctx, q, tenantID, missionID).
		Scan(&m.TenantID, &m.ID, &m.Name, &m.Goal, &m.Description, &m.Status, &m.Queued,
			&m.QueuedAt, &m.StartedAt, &m.FinishedAt, &m.DeadlineAt, &m.Reason,
			&m.MaxTokens, &m.SpentTokens, &m.MaxSteps, &m.StepsUsed, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return Mission{}, fmt.Errorf("读取任务失败: %w", err)
	}
	return m, nil
}

// 任务状态合法值，与数据库 CHECK 约束保持一致。
var validMissionStatuses = map[MissionStatus]bool{
	MissionDraft:     true,
	MissionActive:    true,
	MissionPaused:    true,
	MissionDone:      true,
	MissionFailed:    true,
	MissionCancelled: true,
}

// SetMissionStatus 更新任务状态。
func (s *Store) SetMissionStatus(ctx context.Context, tenantID, missionID int64, status MissionStatus) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if !validMissionStatuses[status] {
		return fmt.Errorf("%w: 非法任务状态 %q", ErrInvalidInput, status)
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE missions SET status = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, missionID, string(status))
	if err != nil {
		return fmt.Errorf("更新任务状态失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("更新任务状态失败: %w", pgx.ErrNoRows)
	}
	return nil
}

// ---------- 任务范围 ----------

// scopeKinds 是允许的任务范围类型。
var scopeKinds = map[string]bool{
	"root_domain": true,
	"subdomain":   true,
	"ip":          true,
	"cidr":        true,
	"url":         true,
	"keyword":     true,
}

// AddMissionScope 批量写入任务范围（幂等 upsert）。
func (s *Store) AddMissionScope(ctx context.Context, tenantID, missionID int64, entries []ScopeEntry) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
		INSERT INTO mission_scope (tenant_id, mission_id, kind, value, allowed_actions, note)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, mission_id, kind, value) DO UPDATE
		SET allowed_actions = EXCLUDED.allowed_actions,
		    note = EXCLUDED.note`
	for _, e := range entries {
		if !scopeKinds[e.Kind] {
			return fmt.Errorf("%w: 非法范围类型 %q", ErrInvalidInput, e.Kind)
		}
		if e.Value == "" {
			return fmt.Errorf("%w: 范围值不能为空", ErrInvalidInput)
		}
		actions := e.AllowedActions
		if actions == nil {
			actions = []string{}
		}
		if _, err := tx.Exec(ctx, q, tenantID, missionID, e.Kind, e.Value, actions, e.Note); err != nil {
			return fmt.Errorf("写入任务范围失败: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交任务范围失败: %w", err)
	}
	return nil
}

// ListMissionScope 读取任务范围。
func (s *Store) ListMissionScope(ctx context.Context, tenantID, missionID int64) ([]ScopeEntry, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	const q = `
		SELECT kind, value, allowed_actions, note
		FROM mission_scope WHERE tenant_id = $1 AND mission_id = $2
		ORDER BY id`
	rows, err := s.db.pool.Query(ctx, q, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("读取任务范围失败: %w", err)
	}
	defer rows.Close()

	var out []ScopeEntry
	for rows.Next() {
		var e ScopeEntry
		if err := rows.Scan(&e.Kind, &e.Value, &e.AllowedActions, &e.Note); err != nil {
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

// LinkMissionAssetsByScope 把任务范围解析为资产并写入 mission_asset_links。
//
// 匹配规则（v1）：
//   - root_domain：根域本身，以及以该域结尾的子域；
//   - subdomain：精确子域；
//   - ip：精确 IP；
//   - cidr：落在网段内的 IP 资产；
//   - url：精确端点；
//   - keyword：只作为提示保存，不解析为资产（避免"看起来生效"的假象）。
//
// 返回新登记（或已存在）的资产链接数量。
func (s *Store) LinkMissionAssetsByScope(ctx context.Context, tenantID, missionID int64) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	entries, err := s.ListMissionScope(ctx, tenantID, missionID)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var linked int64
	for _, e := range entries {
		var query string
		switch e.Kind {
		case "root_domain":
			query = `SELECT id FROM assets
			         WHERE tenant_id = $1 AND (
			             (type = 'root_domain' AND label = $3) OR
			             (type = 'subdomain' AND (label = $3 OR label LIKE '%.' || $3))
			         )`
		case "subdomain", "ip", "url":
			assetType := map[string]string{"subdomain": "subdomain", "ip": "ip", "url": "endpoint"}[e.Kind]
			query = `SELECT id FROM assets WHERE tenant_id = $1 AND type = '` + assetType + `' AND label = $3`
		case "cidr":
			query = `SELECT id FROM assets WHERE tenant_id = $1 AND type = 'ip' AND label::inet <<= $3::cidr`
		case "keyword":
			continue
		default:
			return 0, fmt.Errorf("%w: 未知范围类型 %q", ErrInvalidInput, e.Kind)
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO mission_asset_links (tenant_id, mission_id, asset_id, source)
			 SELECT $1, $2, id, 'scope' FROM (`+query+`) AS matched
			 ON CONFLICT (tenant_id, mission_id, asset_id) DO NOTHING`,
			tenantID, missionID, e.Value)
		if err != nil {
			return 0, fmt.Errorf("登记任务资产失败（%s=%s）: %w", e.Kind, e.Value, err)
		}
		linked += tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交任务资产失败: %w", err)
	}
	return linked, nil
}

// LinkMissionAsset 手动把单个资产登记到任务。
func (s *Store) LinkMissionAsset(ctx context.Context, tenantID, missionID, assetID int64, source string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if source == "" {
		source = "manual"
	}
	_, err := s.db.pool.Exec(ctx,
		`INSERT INTO mission_asset_links (tenant_id, mission_id, asset_id, source)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, mission_id, asset_id) DO NOTHING`,
		tenantID, missionID, assetID, source)
	if err != nil {
		return fmt.Errorf("登记任务资产失败: %w", err)
	}
	return nil
}

// ListMissionAssets 读取任务已登记的资产。
func (s *Store) ListMissionAssets(ctx context.Context, tenantID, missionID int64) ([]Asset, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	const q = `
		SELECT a.tenant_id, a.id, a.type, a.label, a.parent_id, a.attrs, a.created_at, a.updated_at
		FROM assets a
		JOIN mission_asset_links l
		  ON l.tenant_id = a.tenant_id AND l.asset_id = a.id
		WHERE l.tenant_id = $1 AND l.mission_id = $2
		ORDER BY a.id`
	rows, err := s.db.pool.Query(ctx, q, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("读取任务资产失败: %w", err)
	}
	defer rows.Close()

	// 初始化非 nil 切片：HTTP 层需要输出 [] 而不是 null（无资产的任务是正常状态）。
	out := make([]Asset, 0, 8)
	for rows.Next() {
		var a Asset
		var attrs []byte
		if err := rows.Scan(&a.TenantID, &a.ID, &a.Type, &a.Label, &a.ParentID, &attrs, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析任务资产失败: %w", err)
		}
		if a.Attrs, err = decodeJSON(attrs); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历任务资产失败: %w", err)
	}
	return out, nil
}
