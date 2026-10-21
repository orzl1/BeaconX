package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/finding"
)

// 本文件实现 M6.2-B2 的发现 / 发现证据关联 / 复测存储。
//
// 约定与 B1 一致：
//   - 每个方法都显式接收 tenantID，所有 SQL 都带 tenant_id 条件；
//   - 「不存在」统一返回 ErrNotFound，「冲突」统一返回 ErrConflict，
//     「请求体引用非法」统一返回 ErrInvalidInput，调用方用 errors.Is 判断；
//   - 状态迁移先在领域包校验，再在事务内用行锁（SELECT ... FOR UPDATE）读取
//     当前状态做二次校验，避免并发下「先查后写」的竞态；
//   - 返回的切片一律初始化为非 nil，HTTP 层才能稳定输出 [] 而不是 null。

// findingColumns 是 findings 表的裸列（不含 evidence_count 子查询），
// 用于 INSERT/UPDATE 的 RETURNING。
const findingColumns = `tenant_id, id, mission_id, asset_id, node_id, title, severity, status, detail, created_at, updated_at`

// findingSelect 是带 evidence_count 子查询的完整读取语句（需要拼 WHERE/ORDER）。
//
// 证据数量实时统计而不是冗余列：证据绑定/解绑都要在同一事务里更新计数，
// 冗余列一旦漏更新就会长期漂移，而子查询在这个数据量级（单任务几十条）足够快。
const findingSelect = `SELECT f.tenant_id, f.id, f.mission_id, f.asset_id, f.node_id, f.title,
       f.severity, f.status, f.detail,
       (SELECT count(*) FROM finding_evidence fe
         WHERE fe.tenant_id = f.tenant_id AND fe.finding_id = f.id) AS evidence_count,
       f.created_at, f.updated_at
  FROM findings f`

// retestColumns 是 retests 表的裸列。
const retestColumns = `tenant_id, id, mission_id, finding_id, status, note, requested_by, created_at, started_at, finished_at`

// ensureMission 校验任务在当前租户内存在；不存在返回 ErrNotFound。
func (s *Store) ensureMission(ctx context.Context, tenantID, missionID int64) error {
	if missionID <= 0 {
		return fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}
	var exists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM missions WHERE tenant_id = $1 AND id = $2)`,
		tenantID, missionID).Scan(&exists); err != nil {
		return fmt.Errorf("校验任务失败: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w：任务 %d 不存在", ErrNotFound, missionID)
	}
	return nil
}

// ensureNodeInMission 校验节点在当前租户、当前任务内存在。
//
// 发现只能引用本任务的作战图节点；跨任务引用会让「发现 → 图」的溯源链断掉。
func (s *Store) ensureNodeInMission(ctx context.Context, tenantID, missionID, nodeID int64) error {
	var exists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM operation_nodes
		                 WHERE tenant_id = $1 AND mission_id = $2 AND id = $3)`,
		tenantID, missionID, nodeID).Scan(&exists); err != nil {
		return fmt.Errorf("校验作战图节点失败: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w：节点 %d 不存在或不属于该任务", ErrInvalidInput, nodeID)
	}
	return nil
}

// scanFinding 把一行（带 evidence_count）扫描成 Finding。
func scanFinding(row pgx.Row) (Finding, error) {
	var (
		f        Finding
		severity string
		status   string
	)
	err := row.Scan(&f.TenantID, &f.ID, &f.MissionID, &f.AssetID, &f.NodeID, &f.Title,
		&severity, &status, &f.Detail, &f.EvidenceCount, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Finding{}, fmt.Errorf("%w：发现不存在", ErrNotFound)
	}
	if err != nil {
		return Finding{}, fmt.Errorf("读取发现失败: %w", err)
	}
	f.Severity = finding.Severity(severity)
	f.Status = finding.Status(status)
	return f, nil
}

// scanFindingBase 把一行裸列（无 evidence_count）扫描成 Finding。
func scanFindingBase(row pgx.Row) (Finding, error) {
	var (
		f        Finding
		severity string
		status   string
	)
	err := row.Scan(&f.TenantID, &f.ID, &f.MissionID, &f.AssetID, &f.NodeID, &f.Title,
		&severity, &status, &f.Detail, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Finding{}, fmt.Errorf("%w：发现不存在", ErrNotFound)
	}
	if err != nil {
		return Finding{}, fmt.Errorf("读取发现失败: %w", err)
	}
	f.Severity = finding.Severity(severity)
	f.Status = finding.Status(status)
	return f, nil
}

// scanRetest 把一行扫描成 Retest。
func scanRetest(row pgx.Row) (Retest, error) {
	var (
		r      Retest
		status string
	)
	err := row.Scan(&r.TenantID, &r.ID, &r.MissionID, &r.FindingID, &status,
		&r.Note, &r.RequestedBy, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Retest{}, fmt.Errorf("%w：复测不存在", ErrNotFound)
	}
	if err != nil {
		return Retest{}, fmt.Errorf("读取复测失败: %w", err)
	}
	r.Status = finding.RetestStatus(status)
	return r, nil
}

// CreateFinding 在指定任务下创建一条发现。
//
// 初始状态固定为 open，严重级别缺省为 info；mission 必须存在（ErrNotFound），
// asset/node 若提供则必须在同一租户内存在（节点还要求属于同一任务），否则
// 返回 ErrInvalidInput —— 问题出在请求体引用而不是任务本身。
func (s *Store) CreateFinding(ctx context.Context, tenantID, missionID int64, in FindingInput) (Finding, error) {
	if err := checkTenant(tenantID); err != nil {
		return Finding{}, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Finding{}, fmt.Errorf("%w：发现标题不能为空", ErrInvalidInput)
	}
	severity := in.Severity
	if severity == "" {
		severity = finding.SeverityInfo
	}
	if !severity.Valid() {
		return Finding{}, fmt.Errorf("%w：未知的严重级别 %q", ErrInvalidInput, string(severity))
	}
	if err := s.ensureMission(ctx, tenantID, missionID); err != nil {
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
	if in.NodeID != nil {
		if err := s.ensureNodeInMission(ctx, tenantID, missionID, *in.NodeID); err != nil {
			return Finding{}, err
		}
	}

	var (
		f      Finding
		sevRaw string
		staRaw string
	)
	err := s.db.pool.QueryRow(ctx, `
		INSERT INTO findings (tenant_id, mission_id, asset_id, node_id, title, severity, status, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+findingColumns,
		tenantID, missionID, in.AssetID, in.NodeID, title, string(severity), string(finding.StatusOpen), in.Detail).
		Scan(&f.TenantID, &f.ID, &f.MissionID, &f.AssetID, &f.NodeID, &f.Title,
			&sevRaw, &staRaw, &f.Detail, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return Finding{}, fmt.Errorf("创建发现失败: %w", err)
	}
	f.Severity = finding.Severity(sevRaw)
	f.Status = finding.Status(staRaw)
	return f, nil
}

// GetFinding 按 ID 读取发现（含实时证据数量）。
func (s *Store) GetFinding(ctx context.Context, tenantID, findingID int64) (Finding, error) {
	if err := checkTenant(tenantID); err != nil {
		return Finding{}, err
	}
	if findingID <= 0 {
		return Finding{}, fmt.Errorf("%w：发现标识必须为正数", ErrInvalidInput)
	}
	return scanFinding(s.db.pool.QueryRow(ctx,
		findingSelect+` WHERE f.tenant_id = $1 AND f.id = $2`, tenantID, findingID))
}

// ListFindings 按任务列出发现，可选 severity/status 过滤；按创建时间倒序。
//
// 过滤值非法时返回 ErrInvalidInput（由调用方决定 400）；空结果返回空切片
// 而不是 nil，保证 HTTP 层稳定输出 []。
func (s *Store) ListFindings(ctx context.Context, tenantID, missionID int64, severity, status string, limit int) ([]Finding, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if missionID <= 0 {
		return nil, fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}
	severity = strings.TrimSpace(severity)
	status = strings.TrimSpace(status)
	if severity != "" && !finding.Severity(severity).Valid() {
		return nil, fmt.Errorf("%w：未知的严重级别 %q", ErrInvalidInput, severity)
	}
	if status != "" && !finding.Status(status).Valid() {
		return nil, fmt.Errorf("%w：未知的发现状态 %q", ErrInvalidInput, status)
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
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY f.created_at DESC, f.id DESC LIMIT $%d`, len(args))

	rows, err := s.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("列出发现失败: %w", err)
	}
	defer rows.Close()

	out := make([]Finding, 0, 8)
	for rows.Next() {
		var (
			f      Finding
			sevRaw string
			staRaw string
		)
		if err := rows.Scan(&f.TenantID, &f.ID, &f.MissionID, &f.AssetID, &f.NodeID, &f.Title,
			&sevRaw, &staRaw, &f.Detail, &f.EvidenceCount, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析发现失败: %w", err)
		}
		f.Severity = finding.Severity(sevRaw)
		f.Status = finding.Status(staRaw)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历发现失败: %w", err)
	}
	return out, nil
}

// UpdateFinding 局部更新发现（title/severity/detail/asset_id）。
//
// nil 字段保持不变；AssetID 与 ClearAsset 互斥，同时提供视为非法输入。
func (s *Store) UpdateFinding(ctx context.Context, tenantID, findingID int64, in FindingUpdate) (Finding, error) {
	if err := checkTenant(tenantID); err != nil {
		return Finding{}, err
	}
	if findingID <= 0 {
		return Finding{}, fmt.Errorf("%w：发现标识必须为正数", ErrInvalidInput)
	}
	if in.AssetID != nil && in.ClearAsset {
		return Finding{}, fmt.Errorf("%w：asset_id 与清空资产不能同时提供", ErrInvalidInput)
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
	if in.AssetID != nil {
		if err := s.ensureAsset(ctx, tenantID, *in.AssetID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return Finding{}, fmt.Errorf("%w：资产 %d 不存在或不属于当前租户", ErrInvalidInput, *in.AssetID)
			}
			return Finding{}, err
		}
	}

	var updatedID int64
	err := s.db.pool.QueryRow(ctx, `
		UPDATE findings SET
		    title    = COALESCE($3, title),
		    severity = COALESCE($4, severity),
		    detail   = COALESCE($5, detail),
		    asset_id = CASE WHEN $6 THEN NULL
		                    WHEN $7::BIGINT IS NOT NULL THEN $7::BIGINT
		                    ELSE asset_id END
		WHERE tenant_id = $1 AND id = $2
		RETURNING id`,
		tenantID, findingID, in.Title, severityArg, in.Detail, in.ClearAsset, in.AssetID).Scan(&updatedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Finding{}, fmt.Errorf("%w：发现不存在", ErrNotFound)
	}
	if err != nil {
		return Finding{}, fmt.Errorf("更新发现失败: %w", err)
	}
	// 重新读取以带回证据数量，保持与列表/详情一致的返回形状。
	return s.GetFinding(ctx, tenantID, updatedID)
}

// TransitionFinding 按领域状态机迁移发现状态。
//
// 在事务内用 SELECT ... FOR UPDATE 锁住发现行后再校验，避免两个并发请求
// 各自基于旧状态算出「都合法」的结果。非法迁移返回 ErrConflict（HTTP 409）。
func (s *Store) TransitionFinding(ctx context.Context, tenantID, findingID int64, to finding.Status) (Finding, error) {
	if err := checkTenant(tenantID); err != nil {
		return Finding{}, err
	}
	if findingID <= 0 {
		return Finding{}, fmt.Errorf("%w：发现标识必须为正数", ErrInvalidInput)
	}
	if !to.Valid() {
		return Finding{}, fmt.Errorf("%w：未知的发现状态 %q", ErrInvalidInput, string(to))
	}

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return Finding{}, fmt.Errorf("开启发现状态事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var fromRaw string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM findings WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, findingID).Scan(&fromRaw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Finding{}, fmt.Errorf("%w：发现不存在", ErrNotFound)
		}
		return Finding{}, fmt.Errorf("读取发现状态失败: %w", err)
	}
	if err := finding.ValidateTransition(finding.Status(fromRaw), to); err != nil {
		// 同时包装 ErrConflict（HTTP 409）与领域错误（调用方可细分）。
		return Finding{}, fmt.Errorf("%w：%w", ErrConflict, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE findings SET status = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, findingID, string(to)); err != nil {
		return Finding{}, fmt.Errorf("更新发现状态失败: %w", err)
	}
	f, err := scanFinding(tx.QueryRow(ctx,
		findingSelect+` WHERE f.tenant_id = $1 AND f.id = $2`, tenantID, findingID))
	if err != nil {
		return Finding{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Finding{}, fmt.Errorf("提交发现状态失败: %w", err)
	}
	return f, nil
}

// AddFindingEvidence 把一条证据快照绑定到发现。
//
// 快照必须存在、属于同一租户且属于同一任务（否则 ErrInvalidInput）；
// 重复绑定返回 ErrConflict，而不是静默成功——调用方能据此判断是否需要提示。
func (s *Store) AddFindingEvidence(ctx context.Context, tenantID, findingID, snapshotID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if findingID <= 0 || snapshotID <= 0 {
		return fmt.Errorf("%w：发现标识与证据标识必须为正数", ErrInvalidInput)
	}
	var findingMission int64
	err := s.db.pool.QueryRow(ctx,
		`SELECT mission_id FROM findings WHERE tenant_id = $1 AND id = $2`,
		tenantID, findingID).Scan(&findingMission)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w：发现不存在", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("读取发现失败: %w", err)
	}
	var snapshotMission int64
	err = s.db.pool.QueryRow(ctx,
		`SELECT mission_id FROM evidence_snapshots WHERE tenant_id = $1 AND id = $2`,
		tenantID, snapshotID).Scan(&snapshotMission)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w：证据快照 %d 不存在或不属于当前租户", ErrInvalidInput, snapshotID)
	}
	if err != nil {
		return fmt.Errorf("读取证据快照失败: %w", err)
	}
	if snapshotMission != findingMission {
		return fmt.Errorf("%w：证据快照 %d 不属于该发现所在任务", ErrInvalidInput, snapshotID)
	}

	tag, err := s.db.pool.Exec(ctx, `
		INSERT INTO finding_evidence (tenant_id, finding_id, snapshot_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, finding_id, snapshot_id) DO NOTHING`,
		tenantID, findingID, snapshotID)
	if err != nil {
		return fmt.Errorf("绑定发现证据失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：证据快照 %d 已绑定到该发现", ErrConflict, snapshotID)
	}
	return nil
}

// RemoveFindingEvidence 解除发现与证据快照的绑定；未绑定时返回 ErrNotFound。
func (s *Store) RemoveFindingEvidence(ctx context.Context, tenantID, findingID, snapshotID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if findingID <= 0 || snapshotID <= 0 {
		return fmt.Errorf("%w：发现标识与证据标识必须为正数", ErrInvalidInput)
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM finding_evidence WHERE tenant_id = $1 AND finding_id = $2 AND snapshot_id = $3`,
		tenantID, findingID, snapshotID)
	if err != nil {
		return fmt.Errorf("解除发现证据绑定失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：发现 %d 未绑定证据快照 %d", ErrNotFound, findingID, snapshotID)
	}
	return nil
}

// ListFindingEvidence 列出发现绑定的证据快照；发现不存在时返回 ErrNotFound。
func (s *Store) ListFindingEvidence(ctx context.Context, tenantID, findingID int64) ([]FindingEvidence, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if findingID <= 0 {
		return nil, fmt.Errorf("%w：发现标识必须为正数", ErrInvalidInput)
	}
	var exists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM findings WHERE tenant_id = $1 AND id = $2)`,
		tenantID, findingID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("校验发现失败: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w：发现不存在", ErrNotFound)
	}
	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, finding_id, snapshot_id, created_at
		FROM finding_evidence
		WHERE tenant_id = $1 AND finding_id = $2
		ORDER BY created_at, snapshot_id`, tenantID, findingID)
	if err != nil {
		return nil, fmt.Errorf("读取发现证据失败: %w", err)
	}
	defer rows.Close()

	out := make([]FindingEvidence, 0, 4)
	for rows.Next() {
		var e FindingEvidence
		if err := rows.Scan(&e.TenantID, &e.FindingID, &e.SnapshotID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析发现证据失败: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历发现证据失败: %w", err)
	}
	return out, nil
}

// CreateRetest 为某条发现发起一次复测。
//
// 发现必须存在（ErrNotFound）且属于请求路径上的任务（ErrInvalidInput），
// 避免用 A 任务的路径给 B 任务的发现发起复测。初始状态固定为 pending。
func (s *Store) CreateRetest(ctx context.Context, tenantID, missionID, findingID int64, note, requestedBy string) (Retest, error) {
	if err := checkTenant(tenantID); err != nil {
		return Retest{}, err
	}
	if findingID <= 0 {
		return Retest{}, fmt.Errorf("%w：发现标识必须为正数", ErrInvalidInput)
	}
	if err := s.ensureMission(ctx, tenantID, missionID); err != nil {
		return Retest{}, err
	}
	var findingMission int64
	err := s.db.pool.QueryRow(ctx,
		`SELECT mission_id FROM findings WHERE tenant_id = $1 AND id = $2`,
		tenantID, findingID).Scan(&findingMission)
	if errors.Is(err, pgx.ErrNoRows) {
		return Retest{}, fmt.Errorf("%w：发现不存在", ErrNotFound)
	}
	if err != nil {
		return Retest{}, fmt.Errorf("读取发现失败: %w", err)
	}
	if findingMission != missionID {
		return Retest{}, fmt.Errorf("%w：发现 %d 不属于任务 %d", ErrInvalidInput, findingID, missionID)
	}
	requestedBy = strings.TrimSpace(requestedBy)
	if requestedBy == "" {
		requestedBy = "user"
	}

	r, err := scanRetest(s.db.pool.QueryRow(ctx, `
		INSERT INTO retests (tenant_id, mission_id, finding_id, status, note, requested_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+retestColumns,
		tenantID, missionID, findingID, string(finding.RetestPending), strings.TrimSpace(note), requestedBy))
	if err != nil {
		return Retest{}, fmt.Errorf("创建复测失败: %w", err)
	}
	return r, nil
}

// GetRetest 按 ID 读取复测。
func (s *Store) GetRetest(ctx context.Context, tenantID, retestID int64) (Retest, error) {
	if err := checkTenant(tenantID); err != nil {
		return Retest{}, err
	}
	if retestID <= 0 {
		return Retest{}, fmt.Errorf("%w：复测标识必须为正数", ErrInvalidInput)
	}
	return scanRetest(s.db.pool.QueryRow(ctx,
		`SELECT `+retestColumns+` FROM retests WHERE tenant_id = $1 AND id = $2`,
		tenantID, retestID))
}

// ListRetests 按任务列出复测，按发起时间倒序；空结果返回空切片。
func (s *Store) ListRetests(ctx context.Context, tenantID, missionID int64, limit int) ([]Retest, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if missionID <= 0 {
		return nil, fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}
	limit = normalizeLimit(limit, defaultListLimit, maxListLimit)
	rows, err := s.db.pool.Query(ctx, `
		SELECT `+retestColumns+`
		FROM retests
		WHERE tenant_id = $1 AND mission_id = $2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, tenantID, missionID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出复测失败: %w", err)
	}
	defer rows.Close()

	out := make([]Retest, 0, 8)
	for rows.Next() {
		var (
			r      Retest
			status string
		)
		if err := rows.Scan(&r.TenantID, &r.ID, &r.MissionID, &r.FindingID, &status,
			&r.Note, &r.RequestedBy, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, fmt.Errorf("解析复测失败: %w", err)
		}
		r.Status = finding.RetestStatus(status)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历复测失败: %w", err)
	}
	return out, nil
}

// UpdateRetestStatus 按领域状态机迁移复测状态，并维护 started_at / finished_at。
//
// 迁移在事务内用行锁二次校验；非法迁移返回 ErrConflict（HTTP 409）。
// note 非空时更新说明，空字符串视为「不修改」。
func (s *Store) UpdateRetestStatus(ctx context.Context, tenantID, retestID int64, to finding.RetestStatus, note string) (Retest, error) {
	if err := checkTenant(tenantID); err != nil {
		return Retest{}, err
	}
	if retestID <= 0 {
		return Retest{}, fmt.Errorf("%w：复测标识必须为正数", ErrInvalidInput)
	}
	if !to.Valid() {
		return Retest{}, fmt.Errorf("%w：未知的复测状态 %q", ErrInvalidInput, string(to))
	}

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return Retest{}, fmt.Errorf("开启复测状态事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var fromRaw string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM retests WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, retestID).Scan(&fromRaw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Retest{}, fmt.Errorf("%w：复测不存在", ErrNotFound)
		}
		return Retest{}, fmt.Errorf("读取复测状态失败: %w", err)
	}
	if err := finding.ValidateRetestTransition(finding.RetestStatus(fromRaw), to); err != nil {
		return Retest{}, fmt.Errorf("%w：%w", ErrConflict, err)
	}
	r, err := scanRetest(tx.QueryRow(ctx, `
		UPDATE retests SET
		    status      = $3,
		    note        = CASE WHEN $4 = '' THEN note ELSE $4 END,
		    started_at  = CASE WHEN $3 = 'running' THEN COALESCE(started_at, now()) ELSE started_at END,
		    finished_at = CASE WHEN $3 IN ('passed', 'failed', 'error') THEN now() ELSE finished_at END
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+retestColumns,
		tenantID, retestID, string(to), strings.TrimSpace(note)))
	if err != nil {
		return Retest{}, fmt.Errorf("更新复测状态失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Retest{}, fmt.Errorf("提交复测状态失败: %w", err)
	}
	return r, nil
}

// MissionEvidenceStats 统计一个任务下的流量采集记录数与证据快照数（报告用）。
//
// 两个计数都只做 EXISTS 级聚合，数据量受单任务采集上限约束；统计不校验任务
// 是否存在（报告入口已确认任务），任务不存在时返回零值。
func (s *Store) MissionEvidenceStats(ctx context.Context, tenantID, missionID int64) (MissionEvidenceStats, error) {
	if err := checkTenant(tenantID); err != nil {
		return MissionEvidenceStats{}, err
	}
	if missionID <= 0 {
		return MissionEvidenceStats{}, fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}
	var out MissionEvidenceStats
	err := s.db.pool.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM traffic_exchanges WHERE tenant_id = $1 AND mission_id = $2),
		    (SELECT count(*) FROM evidence_snapshots WHERE tenant_id = $1 AND mission_id = $2)`,
		tenantID, missionID).Scan(&out.TrafficCount, &out.SnapshotCount)
	if err != nil {
		return MissionEvidenceStats{}, fmt.Errorf("统计任务证据失败: %w", err)
	}
	return out, nil
}
