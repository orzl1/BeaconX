package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/asset"
)

// UpsertAsset 按身份键写入资产：
//
//   - 任一身份键已存在：复用该资产，补充缺失身份键并更新标签/属性；
//   - 全部身份键都不存在：新建资产并登记全部身份键；
//   - 同一批身份键指向多个不同资产：返回 ErrIdentityConflict（需要人工合并）。
//
// 身份键必须已由 domain/asset 归一化，本方法只做非空校验。
func (s *Store) UpsertAsset(ctx context.Context, tenantID int64, in AssetInput) (Asset, error) {
	if err := checkTenant(tenantID); err != nil {
		return Asset{}, err
	}
	if !in.Type.Valid() {
		return Asset{}, fmt.Errorf("%w: 非法资产类型 %q", ErrInvalidInput, in.Type)
	}
	if strings.TrimSpace(in.Label) == "" {
		return Asset{}, fmt.Errorf("%w: 资产标签不能为空", ErrInvalidInput)
	}
	if len(in.Identities) == 0 {
		return Asset{}, fmt.Errorf("%w: 至少需要一个身份键", ErrInvalidInput)
	}

	// 去重，避免同一批输入重复写入。
	ids := make([]asset.Identity, 0, len(in.Identities))
	seen := map[string]bool{}
	for _, idn := range in.Identities {
		if !idn.Kind.Valid() {
			return Asset{}, fmt.Errorf("%w: 非法身份类型 %q", ErrInvalidInput, idn.Kind)
		}
		if idn.Key == "" {
			return Asset{}, fmt.Errorf("%w: 身份键不能为空", ErrInvalidInput)
		}
		k := string(idn.Kind) + "\x00" + idn.Key
		if seen[k] {
			continue
		}
		seen[k] = true
		ids = append(ids, idn)
	}

	attrs, err := marshalJSON(in.Attrs)
	if err != nil {
		return Asset{}, err
	}

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return Asset{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 父资产不建外键（复合外键的 ON DELETE SET NULL 会把 tenant_id 置空），
	// 因此在写入路径显式校验：父资产必须存在且属于同一租户。
	if in.ParentID != nil {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM assets WHERE tenant_id = $1 AND id = $2)`,
			tenantID, *in.ParentID).Scan(&exists); err != nil {
			return Asset{}, fmt.Errorf("校验父资产失败: %w", err)
		}
		if !exists {
			return Asset{}, fmt.Errorf("%w: 父资产 %d 不存在或不属于当前租户", ErrInvalidInput, *in.ParentID)
		}
	}

	var existingID int64
	for _, idn := range ids {
		var (
			assetID int64
			typ     string
		)
		err := tx.QueryRow(ctx,
			`SELECT asset_id, (SELECT type FROM assets WHERE tenant_id = $1 AND id = asset_id)
			 FROM asset_identity WHERE tenant_id = $1 AND kind = $2 AND key = $3`,
			tenantID, string(idn.Kind), idn.Key).Scan(&assetID, &typ)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Asset{}, fmt.Errorf("查询资产身份失败: %w", err)
		}
		if typ != string(in.Type) {
			return Asset{}, fmt.Errorf("%w: 身份 %s=%s 属于类型 %s，与输入类型 %s 不一致",
				ErrIdentityConflict, idn.Kind, idn.Key, typ, in.Type)
		}
		if existingID == 0 {
			existingID = assetID
		} else if existingID != assetID {
			return Asset{}, fmt.Errorf("%w: 身份 %s=%s 指向资产 %d，而其他身份指向 %d",
				ErrIdentityConflict, idn.Kind, idn.Key, assetID, existingID)
		}
	}

	if existingID == 0 {
		err = tx.QueryRow(ctx,
			`INSERT INTO assets (tenant_id, type, label, parent_id, attrs)
			 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			tenantID, string(in.Type), in.Label, in.ParentID, string(attrs)).Scan(&existingID)
		if err != nil {
			return Asset{}, fmt.Errorf("创建资产失败: %w", err)
		}
	} else {
		_, err = tx.Exec(ctx,
			`UPDATE assets
			 SET label = $3, parent_id = COALESCE($4, parent_id), attrs = $5
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, existingID, in.Label, in.ParentID, string(attrs))
		if err != nil {
			return Asset{}, fmt.Errorf("更新资产失败: %w", err)
		}
	}

	for _, idn := range ids {
		if _, err := tx.Exec(ctx,
			`INSERT INTO asset_identity (tenant_id, asset_id, kind, key)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, kind, key) DO NOTHING`,
			tenantID, existingID, string(idn.Kind), idn.Key); err != nil {
			return Asset{}, fmt.Errorf("写入资产身份失败: %w", err)
		}
	}

	if in.ParentID != nil && *in.ParentID == existingID {
		return Asset{}, fmt.Errorf("%w: 资产不能以自身作为父资产", ErrInvalidInput)
	}

	if err := tx.Commit(ctx); err != nil {
		return Asset{}, fmt.Errorf("提交资产失败: %w", err)
	}
	return s.GetAsset(ctx, tenantID, existingID)
}

// GetAsset 读取资产及其全部身份键。
func (s *Store) GetAsset(ctx context.Context, tenantID, assetID int64) (Asset, error) {
	if err := checkTenant(tenantID); err != nil {
		return Asset{}, err
	}
	var (
		a     Asset
		typ   string
		attrs []byte
	)
	err := s.db.pool.QueryRow(ctx,
		`SELECT tenant_id, id, type, label, parent_id, attrs, created_at, updated_at
		 FROM assets WHERE tenant_id = $1 AND id = $2`,
		tenantID, assetID).
		Scan(&a.TenantID, &a.ID, &typ, &a.Label, &a.ParentID, &attrs, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Asset{}, fmt.Errorf("读取资产失败: %w", err)
	}
	a.Type = asset.Kind(typ)
	if a.Attrs, err = decodeJSON(attrs); err != nil {
		return Asset{}, err
	}

	rows, err := s.db.pool.Query(ctx,
		`SELECT kind, key FROM asset_identity
		 WHERE tenant_id = $1 AND asset_id = $2 ORDER BY kind, key`,
		tenantID, assetID)
	if err != nil {
		return Asset{}, fmt.Errorf("读取资产身份失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key string
		if err := rows.Scan(&kind, &key); err != nil {
			return Asset{}, fmt.Errorf("解析资产身份失败: %w", err)
		}
		a.Identities = append(a.Identities, asset.Identity{Kind: asset.Kind(kind), Key: key})
	}
	if err := rows.Err(); err != nil {
		return Asset{}, fmt.Errorf("遍历资产身份失败: %w", err)
	}
	return a, nil
}

// ResolveAssetByIdentity 按身份键查找资产 ID。
func (s *Store) ResolveAssetByIdentity(ctx context.Context, tenantID int64, kind asset.Kind, key string) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.pool.QueryRow(ctx,
		`SELECT asset_id FROM asset_identity WHERE tenant_id = $1 AND kind = $2 AND key = $3`,
		tenantID, string(kind), key).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("按身份查找资产失败: %w", err)
	}
	return id, nil
}

// ListAssets 按条件列出资产（不含身份键，避免 N+1 查询）。
func (s *Store) ListAssets(ctx context.Context, tenantID int64, f AssetFilter) ([]Asset, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`SELECT tenant_id, id, type, label, parent_id, attrs, created_at, updated_at
		FROM assets WHERE tenant_id = $1`)
	args = append(args, tenantID)

	if f.Type != nil {
		if !f.Type.Valid() {
			return nil, fmt.Errorf("%w: 非法资产类型 %q", ErrInvalidInput, *f.Type)
		}
		args = append(args, string(*f.Type))
		fmt.Fprintf(&sb, " AND type = $%d", len(args))
	}
	if f.ParentID != nil {
		args = append(args, *f.ParentID)
		fmt.Fprintf(&sb, " AND parent_id = $%d", len(args))
	}
	if f.AfterID > 0 {
		args = append(args, f.AfterID)
		fmt.Fprintf(&sb, " AND id > $%d", len(args))
	}
	args = append(args, limit)
	fmt.Fprintf(&sb, " ORDER BY id LIMIT $%d", len(args))

	rows, err := s.db.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("列出资产失败: %w", err)
	}
	defer rows.Close()

	var out []Asset
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
