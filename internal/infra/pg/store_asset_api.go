package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/orzl1/BeaconX/internal/domain/asset"
)

// 本文件补齐 M6.2-B1 需要的资产/企业/范围/任务资产 CRUD。
//
// 约定（与 store.go 一致）：
//   - 每个方法都显式接收 tenantID，所有 SQL 都带 tenant_id 条件；
//   - 「不存在」统一返回 ErrNotFound，「唯一键冲突」统一返回 ErrConflict，
//     调用方用 errors.Is 判断即可，不需要解析错误文本；
//   - 白名单与数据库 CHECK 约束逐项对齐（company_scope.kind、mission_asset_links.source、
//     assets.type、asset_identity.kind），非法值在应用层就拒绝，避免把 500 泄漏成 400。

// 资源级错误分类。
var (
	// ErrNotFound 表示目标资源不存在（或不属于当前租户，二者对外不区分，
	// 避免通过错误码探测其他租户的数据是否存在）。
	ErrNotFound = errors.New("资源不存在")
	// ErrConflict 表示唯一键冲突（slug 重复、范围条目重复、任务资产重复登记等）。
	ErrConflict = errors.New("资源冲突")
)

// CompanyScope 是企业级资产范围条目。
type CompanyScope struct {
	TenantID  int64     `json:"tenant_id"`
	ID        int64     `json:"id"`
	CompanyID int64     `json:"company_id"`
	Kind      string    `json:"kind"`
	Value     string    `json:"value"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// AssetIdentity 是资产的一条身份键。
type AssetIdentity struct {
	TenantID  int64      `json:"tenant_id"`
	AssetID   int64      `json:"asset_id"`
	Kind      asset.Kind `json:"kind"`
	Key       string     `json:"key"`
	CreatedAt time.Time  `json:"created_at"`
}

// AssetUpdate 是资产的局部更新输入。
//
// 用「字段非 nil 才更新」表达 PATCH 语义；ParentID 与 ClearParent 互斥，
// 因为「不传 parent_id」与「显式传 null 清空父资产」是两种不同的意图。
type AssetUpdate struct {
	Label       *string
	ParentID    *int64
	ClearParent bool
	Attrs       map[string]any
}

// AssetSearch 是资产列表的过滤条件（label 模糊 + type 精确）。
//
// 与 store_asset.go 的 AssetFilter 并存：AssetFilter 面向游标翻页（AfterID），
// AssetSearch 面向 HTTP 层的「关键词 + 类型 + limit」查询，二者不共用是为了
// 不改变既有调用方的语义。
type AssetSearch struct {
	Type  *asset.Kind
	Query string
	Limit int
}

// companyScopeKinds 与 company_scope.kind 的 CHECK 约束保持一致。
var companyScopeKinds = map[string]bool{
	"root_domain": true,
	"subdomain":   true,
	"ip":          true,
	"cidr":        true,
	"url":         true,
	"keyword":     true,
	"icp":         true,
}

// companyScopeSources 是企业范围条目的来源白名单；缺省为 manual。
var companyScopeSources = map[string]bool{
	"manual": true,
	"import": true,
}

// missionAssetSources 与 mission_asset_links.source 的 CHECK 约束保持一致。
var missionAssetSources = map[string]bool{
	"scope":      true,
	"discovered": true,
	"manual":     true,
}

// 列表查询的 limit 约定：缺省 50，上限 200（与既有任务列表一致）。
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// normalizeLimit 把调用方传入的 limit 钳制到 [1, maxListLimit]；<=0 时取默认值。
func normalizeLimit(limit, def, max int) int {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	return limit
}

// isUniqueViolation 判断错误是否为 PostgreSQL 唯一键冲突（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// escapeLike 转义 LIKE/ILIKE 模式中的通配符，避免用户输入的 % 与 _ 被当成
// 通配符（例如 q="100%" 不应匹配所有行）。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// ---------- 企业 ----------

// ListCompanies 按名称/slug 模糊搜索企业；q 为空时返回全部（按 id 升序）。
func (s *Store) ListCompanies(ctx context.Context, tenantID int64, q string, limit int) ([]Company, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit = normalizeLimit(limit, defaultListLimit, maxListLimit)
	q = strings.TrimSpace(q)
	pattern := "%" + escapeLike(q) + "%"

	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, name, slug, attrs, created_at, updated_at
		FROM companies
		WHERE tenant_id = $1
		  AND ($2 = '' OR name ILIKE $3 ESCAPE '\' OR slug ILIKE $3 ESCAPE '\')
		ORDER BY id
		LIMIT $4`, tenantID, q, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("列出企业失败: %w", err)
	}
	defer rows.Close()

	out := make([]Company, 0, limit)
	for rows.Next() {
		var (
			c     Company
			attrs []byte
		)
		if err := rows.Scan(&c.TenantID, &c.ID, &c.Name, &c.Slug, &attrs, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析企业失败: %w", err)
		}
		if c.Attrs, err = decodeJSON(attrs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历企业失败: %w", err)
	}
	return out, nil
}

// UpdateCompany 局部更新企业：name/slug 为空串表示保持原值。
//
// 两个字段都为空时按「无变更」处理，直接返回当前行；这样 PATCH {} 是幂等的
// 而不是错误。slug 与同租户其他企业重复时返回 ErrConflict。
func (s *Store) UpdateCompany(ctx context.Context, tenantID, companyID int64, name, slug string) (Company, error) {
	if err := checkTenant(tenantID); err != nil {
		return Company{}, err
	}
	if companyID <= 0 {
		return Company{}, fmt.Errorf("%w: 企业标识必须为正数", ErrInvalidInput)
	}
	name = strings.TrimSpace(name)
	slug = strings.TrimSpace(slug)
	if name == "" && slug == "" {
		return s.GetCompany(ctx, tenantID, companyID)
	}
	if len(name) > 200 || len(slug) > 200 {
		return Company{}, fmt.Errorf("%w: 企业名称与 slug 长度不能超过 200 字节", ErrInvalidInput)
	}

	sets := make([]string, 0, 2)
	args := []any{tenantID, companyID}
	if name != "" {
		args = append(args, name)
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	if slug != "" {
		args = append(args, slug)
		sets = append(sets, fmt.Sprintf("slug = $%d", len(args)))
	}

	var (
		c     Company
		attrs []byte
	)
	err := s.db.pool.QueryRow(ctx, `
		UPDATE companies SET `+strings.Join(sets, ", ")+`
		WHERE tenant_id = $1 AND id = $2
		RETURNING tenant_id, id, name, slug, attrs, created_at, updated_at`,
		args...).
		Scan(&c.TenantID, &c.ID, &c.Name, &c.Slug, &attrs, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Company{}, fmt.Errorf("%w: 企业 %d 不存在", ErrNotFound, companyID)
	}
	if isUniqueViolation(err) {
		return Company{}, fmt.Errorf("%w: slug %q 已被其他企业使用", ErrConflict, slug)
	}
	if err != nil {
		return Company{}, fmt.Errorf("更新企业失败: %w", err)
	}
	if c.Attrs, err = decodeJSON(attrs); err != nil {
		return Company{}, err
	}
	return c, nil
}

// DeleteCompany 删除企业；其范围条目随外键级联删除。
func (s *Store) DeleteCompany(ctx context.Context, tenantID, companyID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM companies WHERE tenant_id = $1 AND id = $2`, tenantID, companyID)
	if err != nil {
		return fmt.Errorf("删除企业失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 企业 %d 不存在", ErrNotFound, companyID)
	}
	return nil
}

// ensureCompany 校验企业在当前租户内存在；不存在返回 ErrNotFound。
func (s *Store) ensureCompany(ctx context.Context, tenantID, companyID int64) error {
	var exists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM companies WHERE tenant_id = $1 AND id = $2)`,
		tenantID, companyID).Scan(&exists); err != nil {
		return fmt.Errorf("校验企业失败: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: 企业 %d 不存在", ErrNotFound, companyID)
	}
	return nil
}

// ensureAsset 校验资产在当前租户内存在；不存在返回 ErrNotFound。
func (s *Store) ensureAsset(ctx context.Context, tenantID, assetID int64) error {
	var exists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM assets WHERE tenant_id = $1 AND id = $2)`,
		tenantID, assetID).Scan(&exists); err != nil {
		return fmt.Errorf("校验资产失败: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: 资产 %d 不存在", ErrNotFound, assetID)
	}
	return nil
}

// ---------- 企业范围 ----------

// ListCompanyScope 读取企业的范围条目（按 id 升序）。
func (s *Store) ListCompanyScope(ctx context.Context, tenantID, companyID int64) ([]CompanyScope, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if err := s.ensureCompany(ctx, tenantID, companyID); err != nil {
		return nil, err
	}
	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, company_id, kind, value, source, created_at
		FROM company_scope
		WHERE tenant_id = $1 AND company_id = $2
		ORDER BY id`, tenantID, companyID)
	if err != nil {
		return nil, fmt.Errorf("读取企业范围失败: %w", err)
	}
	defer rows.Close()

	out := make([]CompanyScope, 0)
	for rows.Next() {
		var item CompanyScope
		if err := rows.Scan(&item.TenantID, &item.ID, &item.CompanyID,
			&item.Kind, &item.Value, &item.Source, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析企业范围失败: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历企业范围失败: %w", err)
	}
	return out, nil
}

// AddCompanyScope 新增一条企业范围条目。
//
// kind 白名单与数据库 CHECK 一致；source 缺省 manual，只允许 manual/import。
// 同一企业下 (kind, value) 重复时返回 ErrConflict。
func (s *Store) AddCompanyScope(ctx context.Context, tenantID, companyID int64, kind, value, source string) (CompanyScope, error) {
	if err := checkTenant(tenantID); err != nil {
		return CompanyScope{}, err
	}
	kind = strings.TrimSpace(kind)
	value = strings.TrimSpace(value)
	source = strings.TrimSpace(source)
	if source == "" {
		source = "manual"
	}
	if !companyScopeKinds[kind] {
		return CompanyScope{}, fmt.Errorf("%w: 非法范围类型 %q", ErrInvalidInput, kind)
	}
	if value == "" {
		return CompanyScope{}, fmt.Errorf("%w: 范围值不能为空", ErrInvalidInput)
	}
	if !companyScopeSources[source] {
		return CompanyScope{}, fmt.Errorf("%w: 非法范围来源 %q（只允许 manual/import）", ErrInvalidInput, source)
	}
	if err := s.ensureCompany(ctx, tenantID, companyID); err != nil {
		return CompanyScope{}, err
	}

	var item CompanyScope
	err := s.db.pool.QueryRow(ctx, `
		INSERT INTO company_scope (tenant_id, company_id, kind, value, source)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING tenant_id, id, company_id, kind, value, source, created_at`,
		tenantID, companyID, kind, value, source).
		Scan(&item.TenantID, &item.ID, &item.CompanyID, &item.Kind, &item.Value, &item.Source, &item.CreatedAt)
	if isUniqueViolation(err) {
		return CompanyScope{}, fmt.Errorf("%w: 该企业下已存在相同的范围条目（%s=%s）", ErrConflict, kind, value)
	}
	if err != nil {
		return CompanyScope{}, fmt.Errorf("新增企业范围失败: %w", err)
	}
	return item, nil
}

// DeleteCompanyScope 删除企业的一条范围条目。
func (s *Store) DeleteCompanyScope(ctx context.Context, tenantID, companyID, scopeID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if err := s.ensureCompany(ctx, tenantID, companyID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM company_scope WHERE tenant_id = $1 AND company_id = $2 AND id = $3`,
		tenantID, companyID, scopeID)
	if err != nil {
		return fmt.Errorf("删除企业范围失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 企业范围条目 %d 不存在", ErrNotFound, scopeID)
	}
	return nil
}

// ---------- 资产 ----------

// SearchAssets 按 label 模糊 + type 精确列出资产（不含身份键，避免 N+1）。
func (s *Store) SearchAssets(ctx context.Context, tenantID int64, f AssetSearch) ([]Asset, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit := normalizeLimit(f.Limit, defaultListLimit, maxListLimit)

	var (
		conds = []string{"tenant_id = $1"}
		args  = []any{tenantID}
	)
	if f.Type != nil {
		if !f.Type.Valid() {
			return nil, fmt.Errorf("%w: 非法资产类型 %q", ErrInvalidInput, *f.Type)
		}
		args = append(args, string(*f.Type))
		conds = append(conds, fmt.Sprintf("type = $%d", len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+escapeLike(q)+"%")
		conds = append(conds, fmt.Sprintf(`label ILIKE $%d ESCAPE '\'`, len(args)))
	}
	args = append(args, limit)

	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, type, label, parent_id, attrs, created_at, updated_at
		FROM assets
		WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY id
		LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("列出资产失败: %w", err)
	}
	defer rows.Close()

	out := make([]Asset, 0, limit)
	for rows.Next() {
		var (
			a     Asset
			typ   string
			attrs []byte
		)
		if err := rows.Scan(&a.TenantID, &a.ID, &typ, &a.Label, &a.ParentID, &attrs, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析资产失败: %w", err)
		}
		a.Type = asset.Kind(typ)
		if a.Attrs, err = decodeJSON(attrs); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资产失败: %w", err)
	}
	return out, nil
}

// CreateAsset 创建一条**不带身份键**的资产。
//
// 带身份键的写入必须走 UpsertAsset（它负责跨身份的去重与冲突判定）；
// 本方法服务于「人工登记、稍后再补身份」的场景，因此拒绝携带身份键，
// 避免出现绕过 upsert 语义的重复资产。
func (s *Store) CreateAsset(ctx context.Context, tenantID int64, in AssetInput) (Asset, error) {
	if err := checkTenant(tenantID); err != nil {
		return Asset{}, err
	}
	if !in.Type.Valid() {
		return Asset{}, fmt.Errorf("%w: 非法资产类型 %q", ErrInvalidInput, in.Type)
	}
	if strings.TrimSpace(in.Label) == "" {
		return Asset{}, fmt.Errorf("%w: 资产标签不能为空", ErrInvalidInput)
	}
	if len(in.Identities) > 0 {
		return Asset{}, fmt.Errorf("%w: 创建资产时不允许携带身份键，请使用 UpsertAsset", ErrInvalidInput)
	}
	attrs, err := marshalJSON(in.Attrs)
	if err != nil {
		return Asset{}, err
	}
	if err := s.checkParent(ctx, tenantID, in.ParentID, 0); err != nil {
		return Asset{}, err
	}

	var id int64
	err = s.db.pool.QueryRow(ctx, `
		INSERT INTO assets (tenant_id, type, label, parent_id, attrs)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		tenantID, string(in.Type), strings.TrimSpace(in.Label), in.ParentID, string(attrs)).Scan(&id)
	if err != nil {
		return Asset{}, fmt.Errorf("创建资产失败: %w", err)
	}
	return s.GetAsset(ctx, tenantID, id)
}

// checkParent 校验父资产的合法性：必须存在、属于同一租户、且不能是自身。
// selfID 为 0 表示新建（尚不存在自身 ID）。
func (s *Store) checkParent(ctx context.Context, tenantID int64, parentID *int64, selfID int64) error {
	if parentID == nil {
		return nil
	}
	if *parentID <= 0 {
		return fmt.Errorf("%w: 父资产标识必须为正数", ErrInvalidInput)
	}
	if selfID > 0 && *parentID == selfID {
		return fmt.Errorf("%w: 资产不能以自身作为父资产", ErrInvalidInput)
	}
	var exists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM assets WHERE tenant_id = $1 AND id = $2)`,
		tenantID, *parentID).Scan(&exists); err != nil {
		return fmt.Errorf("校验父资产失败: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: 父资产 %d 不存在或不属于当前租户", ErrInvalidInput, *parentID)
	}
	return nil
}

// UpdateAsset 局部更新资产：Label/ParentID/Attrs 为 nil 时保持不变。
//
// ClearParent 与 ParentID 互斥：前者把 parent_id 置空，后者指向新的父资产。
func (s *Store) UpdateAsset(ctx context.Context, tenantID, assetID int64, in AssetUpdate) (Asset, error) {
	if err := checkTenant(tenantID); err != nil {
		return Asset{}, err
	}
	if assetID <= 0 {
		return Asset{}, fmt.Errorf("%w: 资产标识必须为正数", ErrInvalidInput)
	}
	if in.ClearParent && in.ParentID != nil {
		return Asset{}, fmt.Errorf("%w: parent_id 与清空父资产不能同时指定", ErrInvalidInput)
	}

	sets := make([]string, 0, 3)
	args := []any{tenantID, assetID}

	if in.Label != nil {
		label := strings.TrimSpace(*in.Label)
		if label == "" {
			return Asset{}, fmt.Errorf("%w: 资产标签不能为空", ErrInvalidInput)
		}
		if len(label) > 1000 {
			return Asset{}, fmt.Errorf("%w: 资产标签长度不能超过 1000 字节", ErrInvalidInput)
		}
		args = append(args, label)
		sets = append(sets, fmt.Sprintf("label = $%d", len(args)))
	}
	if in.ClearParent {
		sets = append(sets, "parent_id = NULL")
	} else if in.ParentID != nil {
		if err := s.checkParent(ctx, tenantID, in.ParentID, assetID); err != nil {
			return Asset{}, err
		}
		args = append(args, *in.ParentID)
		sets = append(sets, fmt.Sprintf("parent_id = $%d", len(args)))
	}
	if in.Attrs != nil {
		raw, err := marshalJSON(in.Attrs)
		if err != nil {
			return Asset{}, err
		}
		args = append(args, string(raw))
		sets = append(sets, fmt.Sprintf("attrs = $%d::jsonb", len(args)))
	}
	if len(sets) == 0 {
		// 没有任何可更新字段：按幂等无变更处理，但仍要确认资产存在。
		return s.GetAsset(ctx, tenantID, assetID)
	}

	tag, err := s.db.pool.Exec(ctx, `
		UPDATE assets SET `+strings.Join(sets, ", ")+`
		WHERE tenant_id = $1 AND id = $2`, args...)
	if err != nil {
		return Asset{}, fmt.Errorf("更新资产失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Asset{}, fmt.Errorf("%w: 资产 %d 不存在", ErrNotFound, assetID)
	}
	return s.GetAsset(ctx, tenantID, assetID)
}

// DeleteAsset 删除资产；其身份键与任务关联随外键级联删除。
//
// 子资产的 parent_id 不建外键（见迁移注释），因此显式置空，避免留下悬空父子引用。
func (s *Store) DeleteAsset(ctx context.Context, tenantID, assetID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM assets WHERE tenant_id = $1 AND id = $2)`,
		tenantID, assetID).Scan(&exists); err != nil {
		return fmt.Errorf("校验资产失败: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: 资产 %d 不存在", ErrNotFound, assetID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE assets SET parent_id = NULL WHERE tenant_id = $1 AND parent_id = $2`,
		tenantID, assetID); err != nil {
		return fmt.Errorf("解除子资产父子关系失败: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM assets WHERE tenant_id = $1 AND id = $2`, tenantID, assetID); err != nil {
		return fmt.Errorf("删除资产失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交删除资产失败: %w", err)
	}
	return nil
}

// ListAssetIdentities 读取资产的全部身份键（按 kind、key 升序）。
func (s *Store) ListAssetIdentities(ctx context.Context, tenantID, assetID int64) ([]AssetIdentity, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if err := s.ensureAsset(ctx, tenantID, assetID); err != nil {
		return nil, err
	}
	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, asset_id, kind, key, created_at
		FROM asset_identity
		WHERE tenant_id = $1 AND asset_id = $2
		ORDER BY kind, key`, tenantID, assetID)
	if err != nil {
		return nil, fmt.Errorf("读取资产身份失败: %w", err)
	}
	defer rows.Close()

	out := make([]AssetIdentity, 0)
	for rows.Next() {
		var (
			item AssetIdentity
			kind string
		)
		if err := rows.Scan(&item.TenantID, &item.AssetID, &kind, &item.Key, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析资产身份失败: %w", err)
		}
		item.Kind = asset.Kind(kind)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资产身份失败: %w", err)
	}
	return out, nil
}

// ---------- 任务资产 ----------

// AddMissionAssetLink 把资产手动登记到任务。
//
// 先校验任务与资产都存在（任务不存在→ErrNotFound；资产不存在→ErrInvalidInput，
// 因为问题出在请求体引用的资产上），再插入关联。重复登记返回 ErrConflict。
func (s *Store) AddMissionAssetLink(ctx context.Context, tenantID, missionID, assetID int64, source string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "manual"
	}
	if !missionAssetSources[source] {
		return fmt.Errorf("%w: 非法资产来源 %q（只允许 scope/discovered/manual）", ErrInvalidInput, source)
	}
	if missionID <= 0 || assetID <= 0 {
		return fmt.Errorf("%w: 任务标识与资产标识必须为正数", ErrInvalidInput)
	}

	var missionExists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM missions WHERE tenant_id = $1 AND id = $2)`,
		tenantID, missionID).Scan(&missionExists); err != nil {
		return fmt.Errorf("校验任务失败: %w", err)
	}
	if !missionExists {
		return fmt.Errorf("%w: 任务 %d 不存在", ErrNotFound, missionID)
	}
	if err := s.ensureAsset(ctx, tenantID, assetID); err != nil {
		// 资产不存在属于请求体引用错误（400）；其他错误（数据库故障）原样上抛。
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: 资产 %d 不存在或不属于当前租户", ErrInvalidInput, assetID)
		}
		return err
	}

	tag, err := s.db.pool.Exec(ctx, `
		INSERT INTO mission_asset_links (tenant_id, mission_id, asset_id, source)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, mission_id, asset_id) DO NOTHING`,
		tenantID, missionID, assetID, source)
	if err != nil {
		return fmt.Errorf("登记任务资产失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 资产 %d 已登记到任务 %d", ErrConflict, assetID, missionID)
	}
	return nil
}

// RemoveMissionAssetLink 解除任务与资产的关联。
func (s *Store) RemoveMissionAssetLink(ctx context.Context, tenantID, missionID, assetID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM mission_asset_links WHERE tenant_id = $1 AND mission_id = $2 AND asset_id = $3`,
		tenantID, missionID, assetID)
	if err != nil {
		return fmt.Errorf("解除任务资产关联失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 任务 %d 未登记资产 %d", ErrNotFound, missionID, assetID)
	}
	return nil
}
