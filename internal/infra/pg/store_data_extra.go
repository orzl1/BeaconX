package pg

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/domain/finding"
)

// B4c 数据补全与游标分页（P-B4-20..26）。
//
// 本文件只补「既有 Store 方法不够用」的部分，不改既有方法签名：
//   - 身份键独立写/删；
//   - 企业 attrs 局部更新；
//   - 发现 node_id 三态更新；
//   - 跨任务发现列表与导出取数；
//   - 任务范围编译预览（纯查询，不落库）；
//   - traffic/audit/llm_usage 与发现列表的 after_id 游标分页。
//
// 游标约定：
//   - 一律按 id 游标，after_id=0 表示首页；
//   - 列表按 id 倒序（新的在前）时取 id < after_id；按 id 升序（资产）时取 id > after_id；
//   - limit <= 0 取默认值，超过上限被钳制（与既有列表一致）。

// ---------- 常量与通用类型 ----------

// maxExportFindings 是 CSV 导出单次最多导出的发现条数。
//
// 导出走内存列表而不是流式游标：发现量级（单任务几十条、租户级数千条）用它足够；
// 上限存在是为了避免异常租户把服务端内存打满。
const maxExportFindings = 10000

// CompanyPatch 是企业的局部更新输入：nil 字段保持不变。
//
// Attrs 传 nil 表示不改属性；ClearAttrs=true 表示把属性清空为 {}。
type CompanyPatch struct {
	Name       *string
	Slug       *string
	Attrs      map[string]any
	ClearAttrs bool
}

// FindingNodePatch 是发现 node_id 的三态更新：Set=设置、Clear=清空、两者都假=不变。
type FindingNodePatch struct {
	Set    bool
	Clear  bool
	NodeID *int64
}

// FindingCrossFilter 是跨任务发现列表/导出的过滤条件。
//
// MissionID=0 表示不限任务；AssetID=0 表示不限资产；AfterID=0 表示首页。
type FindingCrossFilter struct {
	MissionID int64
	Severity  string
	Status    string
	AssetID   int64
	AfterID   int64
	Limit     int
}

// TrafficPageFilter 是全局流量列表的游标过滤条件（id 倒序）。
type TrafficPageFilter struct {
	MissionID int64
	Tool      string
	Phase     string
	AfterID   int64
	Limit     int
}

// AuditPageFilter 是命令历史（audit_events）的游标过滤条件（id 倒序）。
type AuditPageFilter struct {
	MissionID int64
	Tool      string
	Outcome   string
	AfterID   int64
	Limit     int
}

// LLMPageFilter 是 LLM 调用记录的游标过滤条件（id 倒序）。
type LLMPageFilter struct {
	MissionID int64
	Role      string
	Model     string
	AfterID   int64
	Limit     int
}

// CompiledScopeRule 是一条「范围 → 执行期策略」的规则草案。
//
// 预览接口只返回草案，不写 policy_rules：落地需要与策略编辑器确认覆盖顺序、
// 审批动作与不可变规则的交互，属于下一阶段。
type CompiledScopeRule struct {
	Name       string `json:"name"`
	Tool       string `json:"tool"`
	Action     string `json:"action"`
	ArgPattern string `json:"arg_pattern"`
	Priority   int    `json:"priority"`
}

// ScopeCompilation 是任务范围编译的预览结果。
//
// Compiled 与 len(CompiledRules) 相同，单独给出便于前端直接展示数量；
// Notes 说明哪些范围条目无法自动编译（CIDR/关键词/ICP 等）。
type ScopeCompilation struct {
	MissionID     int64               `json:"mission_id"`
	Compiled      int                 `json:"compiled"`
	CompiledRules []CompiledScopeRule `json:"compiled_rules"`
	Notes         []string            `json:"notes"`
}

// ---------- 企业 attrs ----------

// PatchCompany 局部更新企业：nil 字段保持不变；attrs 支持整体替换与清空。
//
// 与 UpdateCompany 的区别：这里能一次原子更新 name/slug/attrs，避免 HTTP PATCH
// 拆成两次写导致「名字改了但属性没改」的半成品状态。
func (s *Store) PatchCompany(ctx context.Context, tenantID, companyID int64, in CompanyPatch) (Company, error) {
	if err := checkTenant(tenantID); err != nil {
		return Company{}, err
	}
	if companyID <= 0 {
		return Company{}, fmt.Errorf("%w：企业标识必须为正数", ErrInvalidInput)
	}
	if in.Attrs != nil && in.ClearAttrs {
		return Company{}, fmt.Errorf("%w：attrs 与清空属性不能同时提供", ErrInvalidInput)
	}

	sets := make([]string, 0, 3)
	args := []any{tenantID, companyID}

	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return Company{}, fmt.Errorf("%w：企业名称不能为空", ErrInvalidInput)
		}
		if len(name) > 200 {
			return Company{}, fmt.Errorf("%w：企业名称长度不能超过 200 字节", ErrInvalidInput)
		}
		args = append(args, name)
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	if in.Slug != nil {
		slug := strings.TrimSpace(*in.Slug)
		if slug == "" {
			return Company{}, fmt.Errorf("%w：企业 slug 不能为空", ErrInvalidInput)
		}
		if len(slug) > 200 {
			return Company{}, fmt.Errorf("%w：企业 slug 长度不能超过 200 字节", ErrInvalidInput)
		}
		args = append(args, slug)
		sets = append(sets, fmt.Sprintf("slug = $%d", len(args)))
	}
	switch {
	case in.ClearAttrs:
		sets = append(sets, "attrs = '{}'::jsonb")
	case in.Attrs != nil:
		raw, err := marshalJSON(in.Attrs)
		if err != nil {
			return Company{}, err
		}
		args = append(args, string(raw))
		sets = append(sets, fmt.Sprintf("attrs = $%d::jsonb", len(args)))
	}
	if len(sets) == 0 {
		// 没有可更新字段：PATCH {} 按幂等无变更处理，但仍确认企业存在。
		return s.GetCompany(ctx, tenantID, companyID)
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
		return Company{}, fmt.Errorf("%w：企业 %d", ErrNotFound, companyID)
	}
	if isUniqueViolation(err) {
		return Company{}, fmt.Errorf("%w：slug 已被其他企业使用", ErrConflict)
	}
	if err != nil {
		return Company{}, fmt.Errorf("更新企业失败: %w", err)
	}
	if c.Attrs, err = decodeJSON(attrs); err != nil {
		return Company{}, err
	}
	return c, nil
}

// ---------- 资产身份键 ----------

// AddAssetIdentity 给已有资产追加一条身份键。
//
// 调用方必须已经用 domain/asset.Normalize 归一化；这里只做非空与类型校验。
// 身份键已存在（无论属于当前资产还是别的资产）都返回 ErrConflict：
// 唯一键冲突必须让调用方显式处理，而不是静默幂等。
func (s *Store) AddAssetIdentity(ctx context.Context, tenantID, assetID int64, idn asset.Identity) (AssetIdentity, error) {
	if err := checkTenant(tenantID); err != nil {
		return AssetIdentity{}, err
	}
	if assetID <= 0 {
		return AssetIdentity{}, fmt.Errorf("%w：资产标识必须为正数", ErrInvalidInput)
	}
	if !idn.Kind.Valid() {
		return AssetIdentity{}, fmt.Errorf("%w：非法身份类型 %q", ErrInvalidInput, string(idn.Kind))
	}
	if strings.TrimSpace(idn.Key) == "" {
		return AssetIdentity{}, fmt.Errorf("%w：身份键不能为空", ErrInvalidInput)
	}
	if err := s.ensureAsset(ctx, tenantID, assetID); err != nil {
		return AssetIdentity{}, err
	}

	var (
		out  AssetIdentity
		kind string
	)
	err := s.db.pool.QueryRow(ctx, `
		INSERT INTO asset_identity (tenant_id, asset_id, kind, key)
		VALUES ($1, $2, $3, $4)
		RETURNING tenant_id, asset_id, kind, key, created_at`,
		tenantID, assetID, string(idn.Kind), idn.Key).
		Scan(&out.TenantID, &out.AssetID, &kind, &out.Key, &out.CreatedAt)
	if isUniqueViolation(err) {
		// 区分「同一资产重复添加」与「键已被别的资产占用」，错误文案对前端更有用。
		var owner int64
		queryErr := s.db.pool.QueryRow(ctx,
			`SELECT asset_id FROM asset_identity WHERE tenant_id = $1 AND kind = $2 AND key = $3`,
			tenantID, string(idn.Kind), idn.Key).Scan(&owner)
		if queryErr == nil && owner != assetID {
			return AssetIdentity{}, fmt.Errorf("%w：身份键已被资产 %d 占用", ErrConflict, owner)
		}
		return AssetIdentity{}, fmt.Errorf("%w：该资产已存在相同身份键", ErrConflict)
	}
	if err != nil {
		return AssetIdentity{}, fmt.Errorf("写入资产身份失败: %w", err)
	}
	out.Kind = asset.Kind(kind)
	return out, nil
}

// DeleteAssetIdentity 删除资产的一条身份键；不存在返回 ErrNotFound。
//
// 允许删到零条身份（裸资产是合法形态），因此这里不做「至少保留一条」的限制。
func (s *Store) DeleteAssetIdentity(ctx context.Context, tenantID, assetID int64, idn asset.Identity) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if assetID <= 0 {
		return fmt.Errorf("%w：资产标识必须为正数", ErrInvalidInput)
	}
	if !idn.Kind.Valid() {
		return fmt.Errorf("%w：非法身份类型 %q", ErrInvalidInput, string(idn.Kind))
	}
	tag, err := s.db.pool.Exec(ctx, `
		DELETE FROM asset_identity
		WHERE tenant_id = $1 AND asset_id = $2 AND kind = $3 AND key = $4`,
		tenantID, assetID, string(idn.Kind), idn.Key)
	if err != nil {
		return fmt.Errorf("删除资产身份失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：资产 %d 不存在该身份键", ErrNotFound, assetID)
	}
	return nil
}

// ---------- 发现：node_id 与跨任务列表 ----------

// PatchFinding 局部更新发现，并支持 node_id 三态。
//
// 与 UpdateFinding 的区别：node_id 的校验需要先知道发现所属任务，因此这里先读
// 发现再校验节点是否属于同一任务，最后用一条 UPDATE 原子写入全部字段。
func (s *Store) PatchFinding(ctx context.Context, tenantID, findingID int64, in FindingUpdate, node FindingNodePatch) (Finding, error) {
	if err := checkTenant(tenantID); err != nil {
		return Finding{}, err
	}
	if findingID <= 0 {
		return Finding{}, fmt.Errorf("%w：发现标识必须为正数", ErrInvalidInput)
	}
	if in.AssetID != nil && in.ClearAsset {
		return Finding{}, fmt.Errorf("%w：asset_id 与清空资产不能同时提供", ErrInvalidInput)
	}
	if node.Set && node.Clear {
		return Finding{}, fmt.Errorf("%w：node_id 与清空节点不能同时提供", ErrInvalidInput)
	}
	if node.Set && (node.NodeID == nil || *node.NodeID <= 0) {
		return Finding{}, fmt.Errorf("%w：node_id 必须为正整数", ErrInvalidInput)
	}
	if in.Title != nil {
		title := strings.TrimSpace(*in.Title)
		if title == "" {
			return Finding{}, fmt.Errorf("%w：发现标题不能为空", ErrInvalidInput)
		}
		in.Title = &title
	}
	var severityArg *string
	if in.Severity != nil {
		if !in.Severity.Valid() {
			return Finding{}, fmt.Errorf("%w：未知的严重级别 %q", ErrInvalidInput, string(*in.Severity))
		}
		raw := string(*in.Severity)
		severityArg = &raw
	}

	current, err := s.GetFinding(ctx, tenantID, findingID)
	if err != nil {
		return Finding{}, err
	}
	if in.AssetID != nil {
		if err := s.ensureAsset(ctx, tenantID, *in.AssetID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return Finding{}, fmt.Errorf("%w：资产 %d 不存在或不属于当前租户", ErrInvalidInput, *in.AssetID)
			}
			return Finding{}, err
		}
	}
	if node.Set {
		// 发现只能引用本任务的作战图节点，跨任务引用必须拒绝（400）。
		if err := s.ensureNodeInMission(ctx, tenantID, current.MissionID, *node.NodeID); err != nil {
			return Finding{}, err
		}
	}

	var updatedID int64
	err = s.db.pool.QueryRow(ctx, `
		UPDATE findings SET
		    title    = COALESCE($3, title),
		    severity = COALESCE($4, severity),
		    detail   = COALESCE($5, detail),
		    asset_id = CASE WHEN $6 THEN NULL
		                    WHEN $7::BIGINT IS NOT NULL THEN $7::BIGINT
		                    ELSE asset_id END,
		    node_id  = CASE WHEN $8 THEN NULL
		                    WHEN $9::BIGINT IS NOT NULL THEN $9::BIGINT
		                    ELSE node_id END
		WHERE tenant_id = $1 AND id = $2
		RETURNING id`,
		tenantID, findingID, in.Title, severityArg, in.Detail,
		in.ClearAsset, in.AssetID, node.Clear, node.NodeID).Scan(&updatedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Finding{}, fmt.Errorf("%w：发现 %d", ErrNotFound, findingID)
	}
	if err != nil {
		return Finding{}, fmt.Errorf("更新发现失败: %w", err)
	}
	return s.GetFinding(ctx, tenantID, updatedID)
}

// ListFindingsPage 是任务内发现列表的游标版本（id 倒序，after_id 表示 id < after_id）。
//
// 游标模式统一按 id 倒序：原来的 created_at DESC, id DESC 在「同一秒内创建多条」时
// 无法给出稳定边界，而 id 严格单调，能保证翻页不重不漏。
func (s *Store) ListFindingsPage(ctx context.Context, tenantID, missionID int64, severity, status string, afterID int64, limit int) ([]Finding, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if missionID <= 0 {
		return nil, fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}
	severity, status, err := validateFindingFilters(severity, status)
	if err != nil {
		return nil, err
	}
	limit = normalizeLimit(limit, defaultListLimit, maxListLimit)

	q := findingSelect + ` WHERE f.tenant_id = $1 AND f.mission_id = $2`
	args := []any{tenantID, missionID}
	if severity != "" {
		args = append(args, severity)
		q += fmt.Sprintf(` AND f.severity = $%d`, len(args))
	}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(` AND f.status = $%d`, len(args))
	}
	if afterID > 0 {
		args = append(args, afterID)
		q += fmt.Sprintf(` AND f.id < $%d`, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY f.id DESC LIMIT $%d`, len(args))

	return s.queryFindings(ctx, q, args...)
}

// ListFindingsCross 是跨任务发现列表（id 倒序，after_id 表示 id < after_id）。
func (s *Store) ListFindingsCross(ctx context.Context, tenantID int64, f FindingCrossFilter) ([]Finding, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	severity, status, err := validateFindingFilters(f.Severity, f.Status)
	if err != nil {
		return nil, err
	}
	limit := normalizeLimit(f.Limit, defaultListLimit, maxListLimit)

	q := findingSelect + ` WHERE f.tenant_id = $1`
	args := []any{tenantID}
	if f.MissionID > 0 {
		args = append(args, f.MissionID)
		q += fmt.Sprintf(` AND f.mission_id = $%d`, len(args))
	}
	if severity != "" {
		args = append(args, severity)
		q += fmt.Sprintf(` AND f.severity = $%d`, len(args))
	}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(` AND f.status = $%d`, len(args))
	}
	if f.AssetID > 0 {
		args = append(args, f.AssetID)
		q += fmt.Sprintf(` AND f.asset_id = $%d`, len(args))
	}
	if f.AfterID > 0 {
		args = append(args, f.AfterID)
		q += fmt.Sprintf(` AND f.id < $%d`, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY f.id DESC LIMIT $%d`, len(args))

	return s.queryFindings(ctx, q, args...)
}

// ExportFindings 取出一批用于 CSV 导出的发现（id 倒序，最多 maxExportFindings 条）。
func (s *Store) ExportFindings(ctx context.Context, tenantID int64, f FindingCrossFilter) ([]Finding, error) {
	limit := f.Limit
	if limit <= 0 || limit > maxExportFindings {
		limit = maxExportFindings
	}
	f.Limit = limit
	return s.ListFindingsCross(ctx, tenantID, f)
}

// validateFindingFilters 统一校验并 trim 严重级别与状态过滤值。
func validateFindingFilters(severity, status string) (string, string, error) {
	severity = strings.TrimSpace(severity)
	status = strings.TrimSpace(status)
	if severity != "" && !finding.Severity(severity).Valid() {
		return "", "", fmt.Errorf("%w：未知的严重级别 %q", ErrInvalidInput, severity)
	}
	if status != "" && !finding.Status(status).Valid() {
		return "", "", fmt.Errorf("%w：未知的发现状态 %q", ErrInvalidInput, status)
	}
	return severity, status, nil
}

// queryFindings 执行发现查询并扫描结果（复用 findingSelect 的列顺序）。
func (s *Store) queryFindings(ctx context.Context, q string, args ...any) ([]Finding, error) {
	rows, err := s.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("列出发现失败: %w", err)
	}
	defer rows.Close()

	out := make([]Finding, 0, 8)
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历发现失败: %w", err)
	}
	return out, nil
}

// ---------- 洞察列表的游标分页 ----------

// ListTrafficPage 按游标列出流量采集记录（id 倒序）。
func (s *Store) ListTrafficPage(ctx context.Context, tenantID int64, f TrafficPageFilter) ([]TrafficExchange, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if f.Phase != "" && f.Phase != "intent" && f.Phase != "result" {
		return nil, fmt.Errorf("%w：phase 只能是 intent 或 result", ErrInvalidInput)
	}
	limit := insightsLimit(f.Limit, defaultInsightsListLimit, maxTrafficListLimit)

	conds := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if f.MissionID > 0 {
		args = append(args, f.MissionID)
		conds = append(conds, fmt.Sprintf("mission_id = $%d", len(args)))
	}
	if f.Tool != "" {
		args = append(args, f.Tool)
		conds = append(conds, fmt.Sprintf("tool = $%d", len(args)))
	}
	if f.Phase != "" {
		args = append(args, f.Phase)
		conds = append(conds, fmt.Sprintf("phase = $%d", len(args)))
	}
	if f.AfterID > 0 {
		args = append(args, f.AfterID)
		conds = append(conds, fmt.Sprintf("id < $%d", len(args)))
	}
	args = append(args, limit)
	q := `SELECT ` + trafficColumns + ` FROM traffic_exchanges WHERE ` +
		strings.Join(conds, " AND ") + fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := s.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("读取流量列表失败: %w", err)
	}
	defer rows.Close()

	out := []TrafficExchange{}
	for rows.Next() {
		item, err := scanTraffic(rows)
		if err != nil {
			return nil, fmt.Errorf("解析流量记录失败: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历流量记录失败: %w", err)
	}
	return out, nil
}

// ListAuditPage 按游标列出命令执行审计（id 倒序）。
func (s *Store) ListAuditPage(ctx context.Context, tenantID int64, f AuditPageFilter) ([]AuditEvent, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit := insightsLimit(f.Limit, defaultInsightsListLimit, maxAuditListLimit)

	conds := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if f.MissionID > 0 {
		args = append(args, f.MissionID)
		conds = append(conds, fmt.Sprintf("mission_id = $%d", len(args)))
	}
	if f.Tool != "" {
		args = append(args, f.Tool)
		conds = append(conds, fmt.Sprintf("tool = $%d", len(args)))
	}
	if f.Outcome != "" {
		args = append(args, f.Outcome)
		conds = append(conds, fmt.Sprintf("outcome = $%d", len(args)))
	}
	if f.AfterID > 0 {
		args = append(args, f.AfterID)
		conds = append(conds, fmt.Sprintf("id < $%d", len(args)))
	}
	args = append(args, limit)
	q := `SELECT ` + auditEventColumns + ` FROM audit_events WHERE ` +
		strings.Join(conds, " AND ") + fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := s.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("读取命令列表失败: %w", err)
	}
	defer rows.Close()

	out := []AuditEvent{}
	for rows.Next() {
		item, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历命令记录失败: %w", err)
	}
	return out, nil
}

// ListLLMPage 按游标列出模型调用记录（id 倒序）。
func (s *Store) ListLLMPage(ctx context.Context, tenantID int64, f LLMPageFilter) ([]LLMUsageRecord, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit := insightsLimit(f.Limit, defaultInsightsListLimit, maxLLMRecordListLimit)

	conds := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if f.MissionID > 0 {
		args = append(args, f.MissionID)
		conds = append(conds, fmt.Sprintf("mission_id = $%d", len(args)))
	}
	if f.Role != "" {
		args = append(args, f.Role)
		conds = append(conds, fmt.Sprintf("role = $%d", len(args)))
	}
	if f.Model != "" {
		args = append(args, f.Model)
		conds = append(conds, fmt.Sprintf("model = $%d", len(args)))
	}
	if f.AfterID > 0 {
		args = append(args, f.AfterID)
		conds = append(conds, fmt.Sprintf("id < $%d", len(args)))
	}
	args = append(args, limit)
	q := `SELECT ` + llmUsageColumns + ` FROM llm_usage WHERE ` +
		strings.Join(conds, " AND ") + fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := s.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("读取 LLM 记录失败: %w", err)
	}
	defer rows.Close()

	out := []LLMUsageRecord{}
	for rows.Next() {
		record, err := scanLLMUsageRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 LLM 记录失败: %w", err)
	}
	return out, nil
}

// ---------- 任务范围编译预览 ----------

// compileScopePriorities 是不同范围类型的规则优先级：越具体的范围优先级越高，
// 这样策略引擎按 priority 降序匹配时，URL/子域规则会先于根域规则命中。
var compileScopePriorities = map[string]int{
	"url":         120,
	"subdomain":   110,
	"ip":          110,
	"root_domain": 100,
}

// CompileMissionScope 把任务范围编译成执行期策略规则草案（**只预览，不落库**）。
//
// 只对能够用正则表达地址边界的类型生成规则：
//   - root_domain：允许该域及其任意子域；
//   - subdomain / ip：允许该主机；
//   - url：允许该 URL（可带查询串）。
//
// cidr / keyword / icp 无法用单条正则精确表达，只写 notes 说明，不生成规则。
// 任务不存在返回 ErrNotFound；范围为空时返回空规则 + 说明性 notes。
func (s *Store) CompileMissionScope(ctx context.Context, tenantID, missionID int64) (ScopeCompilation, error) {
	if err := checkTenant(tenantID); err != nil {
		return ScopeCompilation{}, err
	}
	if err := s.ensureMission(ctx, tenantID, missionID); err != nil {
		return ScopeCompilation{}, err
	}
	entries, err := s.ListMissionScopeEntries(ctx, tenantID, missionID)
	if err != nil {
		return ScopeCompilation{}, err
	}

	out := ScopeCompilation{
		MissionID:     missionID,
		CompiledRules: []CompiledScopeRule{},
		Notes:         []string{},
	}
	if len(entries) == 0 {
		out.Notes = append(out.Notes, "任务范围为空，未生成任何规则；请先添加范围条目")
		return out, nil
	}

	for _, entry := range entries {
		rule, ok := compileScopeRule(entry.Kind, entry.Value)
		if ok {
			out.CompiledRules = append(out.CompiledRules, rule)
			continue
		}
		switch entry.Kind {
		case "cidr":
			out.Notes = append(out.Notes, fmt.Sprintf(
				"范围 cidr=%s 无法编译为单条正则（网段需在策略层按地址段单独配置），已跳过", entry.Value))
		case "keyword", "icp":
			out.Notes = append(out.Notes, fmt.Sprintf(
				"范围 %s=%s 不是可解析的网络地址，仅作为规划提示保留，未生成规则", entry.Kind, entry.Value))
		default:
			out.Notes = append(out.Notes, fmt.Sprintf(
				"范围 %s=%s 暂不支持编译，已跳过", entry.Kind, entry.Value))
		}
	}
	out.Compiled = len(out.CompiledRules)
	return out, nil
}

// compileScopeRule 把一条范围条目编译为 http_probe 的 allow 规则草案。
//
// 返回 ok=false 表示该类型无法用正则表达，需要由调用方补 note。
func compileScopeRule(kind, value string) (CompiledScopeRule, bool) {
	kind = strings.TrimSpace(kind)
	value = strings.TrimSpace(value)
	if value == "" {
		return CompiledScopeRule{}, false
	}
	priority, known := compileScopePriorities[kind]
	if !known {
		return CompiledScopeRule{}, false
	}
	escaped := regexp.QuoteMeta(value)
	var (
		argPattern string
		label      string
	)
	switch kind {
	case "root_domain":
		argPattern = `(?i)^https?://([a-z0-9_-]+\.)*` + escaped + `(:\d{1,5})?(/|$)`
		label = "根域 " + value + "（含任意子域）"
	case "subdomain":
		argPattern = `(?i)^https?://` + escaped + `(:\d{1,5})?(/|$)`
		label = "子域 " + value
	case "ip":
		host := escaped
		if strings.Contains(value, ":") {
			// IPv6 在 URL 中必须带方括号，转义后补齐，避免规则匹配不到。
			host = `\[` + escaped + `\]`
		}
		argPattern = `(?i)^https?://` + host + `(:\d{1,5})?(/|$)`
		label = "IP " + value
	case "url":
		argPattern = `(?i)^` + escaped + `(\?.*)?$`
		label = "URL " + value
	}
	return CompiledScopeRule{
		Name:       "允许 http_probe 访问" + label,
		Tool:       "http_probe",
		Action:     "allow",
		ArgPattern: argPattern,
		Priority:   priority,
	}, true
}
