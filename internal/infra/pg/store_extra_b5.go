package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 本文件实现 B5 批次需要的增量存储：证据访问审计、任务级流量清理、发现-节点
// 关联查询与「发现的最新证据」查询。
//
// 约定与其它 store 一致：所有方法显式接收 tenantID、所有 SQL 带 tenant_id；
// 不存在统一返回 ErrNotFound，参数非法返回 ErrInvalidInput，返回切片非 nil。

// evidenceAccessActions 与 0014 的 CHECK 约束保持一致。
var evidenceAccessActions = map[string]bool{
	"read":   true,
	"verify": true,
	"export": true,
}

// EvidenceAccessLog 是一条证据访问审计记录。
type EvidenceAccessLog struct {
	TenantID   int64     `json:"tenant_id"`
	ID         int64     `json:"id"`
	SnapshotID int64     `json:"snapshot_id"`
	Action     string    `json:"action"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}

const evidenceAccessColumns = `tenant_id, id, snapshot_id, action, actor, created_at`

// isEvidenceForeignKeyViolation 判断错误是否为外键冲突（SQLSTATE 23503）。
func isEvidenceForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// scanEvidenceAccess 扫描一行审计记录。
func scanEvidenceAccess(row pgx.Row) (EvidenceAccessLog, error) {
	var entry EvidenceAccessLog
	if err := row.Scan(&entry.TenantID, &entry.ID, &entry.SnapshotID, &entry.Action,
		&entry.Actor, &entry.CreatedAt); err != nil {
		return EvidenceAccessLog{}, err
	}
	return entry, nil
}

// LogEvidenceAccess 追加一条证据访问审计。
//
// action 只允许 read/verify/export（与表 CHECK 一致）；snapshot 不存在时返回
// ErrNotFound（外键冲突），避免把审计挂到不存在的对象上。
func (s *Store) LogEvidenceAccess(ctx context.Context, tenantID, snapshotID int64, action, actor string) (EvidenceAccessLog, error) {
	if err := checkTenant(tenantID); err != nil {
		return EvidenceAccessLog{}, err
	}
	if snapshotID <= 0 {
		return EvidenceAccessLog{}, fmt.Errorf("%w：证据标识必须为正数", ErrInvalidInput)
	}
	if !evidenceAccessActions[action] {
		return EvidenceAccessLog{}, fmt.Errorf("%w：证据访问动作必须是 read/verify/export", ErrInvalidInput)
	}
	entry, err := scanEvidenceAccess(s.db.pool.QueryRow(ctx, `
		INSERT INTO evidence_access_log (tenant_id, snapshot_id, action, actor)
		VALUES ($1, $2, $3, $4)
		RETURNING `+evidenceAccessColumns,
		tenantID, snapshotID, action, actor))
	if isEvidenceForeignKeyViolation(err) {
		return EvidenceAccessLog{}, fmt.Errorf("%w：证据快照不存在", ErrNotFound)
	}
	if err != nil {
		return EvidenceAccessLog{}, fmt.Errorf("写入证据访问审计失败: %w", err)
	}
	return entry, nil
}

// ListEvidenceAccess 按 id 倒序列出某条证据快照的访问审计。
//
// 快照不存在时返回 ErrNotFound；存在但无记录时返回空切片。
func (s *Store) ListEvidenceAccess(ctx context.Context, tenantID, snapshotID int64, limit int) ([]EvidenceAccessLog, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if snapshotID <= 0 {
		return nil, fmt.Errorf("%w：证据标识必须为正数", ErrInvalidInput)
	}
	var exists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM evidence_snapshots WHERE tenant_id = $1 AND id = $2)`,
		tenantID, snapshotID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("校验证据快照失败: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w：证据快照不存在", ErrNotFound)
	}
	limit = normalizeLimit(limit, defaultListLimit, maxListLimit)
	rows, err := s.db.pool.Query(ctx, `
		SELECT `+evidenceAccessColumns+`
		FROM evidence_access_log
		WHERE tenant_id = $1 AND snapshot_id = $2
		ORDER BY id DESC LIMIT $3`, tenantID, snapshotID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取证据访问审计失败: %w", err)
	}
	defer rows.Close()

	out := make([]EvidenceAccessLog, 0, 8)
	for rows.Next() {
		var entry EvidenceAccessLog
		if err := rows.Scan(&entry.TenantID, &entry.ID, &entry.SnapshotID, &entry.Action,
			&entry.Actor, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析证据访问审计失败: %w", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历证据访问审计失败: %w", err)
	}
	return out, nil
}

// DeleteMissionTraffic 删除任务的全部流量采集行（traffic_exchanges），返回删除数。
//
// 只删采集行，不动 evidence_snapshots 与其正文：证据是已冻结的独立对象，任务级
// 清理不能让已绑定的证据失效。
func (s *Store) DeleteMissionTraffic(ctx context.Context, tenantID, missionID int64) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	if missionID <= 0 {
		return 0, fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM traffic_exchanges WHERE tenant_id = $1 AND mission_id = $2`,
		tenantID, missionID)
	if err != nil {
		return 0, fmt.Errorf("清理任务流量失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// FindingIDForNode 返回某个作战图节点已关联的发现 ID；没有时返回 (0,false,nil)。
//
// findings.node_id 没有数据库唯一约束（历史数据可能已存在重复），因此这里取
// 最小 id 作为「该节点的代表发现」，供「从节点创建发现」做 409 幂等判断。
func (s *Store) FindingIDForNode(ctx context.Context, tenantID, nodeID int64) (int64, bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, false, err
	}
	if nodeID <= 0 {
		return 0, false, fmt.Errorf("%w：节点标识必须为正数", ErrInvalidInput)
	}
	var id int64
	err := s.db.pool.QueryRow(ctx,
		`SELECT id FROM findings WHERE tenant_id = $1 AND node_id = $2 ORDER BY id LIMIT 1`,
		tenantID, nodeID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查询节点关联发现失败: %w", err)
	}
	return id, true, nil
}

// scanEvidenceSnapshot 扫描一行证据快照。
func scanEvidenceSnapshot(row pgx.Row) (EvidenceSnapshot, error) {
	var snap EvidenceSnapshot
	err := row.Scan(&snap.TenantID, &snap.ID, &snap.MissionID, &snap.Tool,
		&snap.RequestHash, &snap.RequestSize, &snap.ResponseHash, &snap.ResponseSize,
		&snap.Signature, &snap.Redaction, &snap.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return EvidenceSnapshot{}, fmt.Errorf("%w：证据快照不存在", ErrNotFound)
	}
	if err != nil {
		return EvidenceSnapshot{}, fmt.Errorf("读取证据快照失败: %w", err)
	}
	return snap, nil
}

// LatestFindingEvidence 返回发现最近绑定的一条证据快照（按快照创建时间倒序）。
//
// 没有绑定证据时返回 ErrNotFound；复测执行用它判断「无证据」分支。
func (s *Store) LatestFindingEvidence(ctx context.Context, tenantID, findingID int64) (EvidenceSnapshot, error) {
	if err := checkTenant(tenantID); err != nil {
		return EvidenceSnapshot{}, err
	}
	if findingID <= 0 {
		return EvidenceSnapshot{}, fmt.Errorf("%w：发现标识必须为正数", ErrInvalidInput)
	}
	return scanEvidenceSnapshot(s.db.pool.QueryRow(ctx, `
		SELECT s.`+`tenant_id, s.id, s.mission_id, s.tool, s.request_hash, s.request_size,
		       s.response_hash, s.response_size, s.signature, s.redaction, s.created_at
		FROM finding_evidence b
		JOIN evidence_snapshots s
		  ON s.tenant_id = b.tenant_id AND s.id = b.snapshot_id
		WHERE b.tenant_id = $1 AND b.finding_id = $2
		ORDER BY s.created_at DESC, s.id DESC
		LIMIT 1`, tenantID, findingID))
}

// ListRetestEvidence 列出复测关联发现绑定的全部证据快照（按快照创建时间升序）。
//
// 复测不存在时返回 ErrNotFound；没有证据时返回空切片。
func (s *Store) ListRetestEvidence(ctx context.Context, tenantID, retestID int64) ([]EvidenceSnapshot, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if retestID <= 0 {
		return nil, fmt.Errorf("%w：复测标识必须为正数", ErrInvalidInput)
	}
	var findingID int64
	err := s.db.pool.QueryRow(ctx,
		`SELECT finding_id FROM retests WHERE tenant_id = $1 AND id = $2`,
		tenantID, retestID).Scan(&findingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w：复测不存在", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("读取复测失败: %w", err)
	}

	rows, err := s.db.pool.Query(ctx, `
		SELECT s.`+`tenant_id, s.id, s.mission_id, s.tool, s.request_hash, s.request_size,
		       s.response_hash, s.response_size, s.signature, s.redaction, s.created_at
		FROM finding_evidence b
		JOIN evidence_snapshots s
		  ON s.tenant_id = b.tenant_id AND s.id = b.snapshot_id
		WHERE b.tenant_id = $1 AND b.finding_id = $2
		ORDER BY s.created_at, s.id`, tenantID, findingID)
	if err != nil {
		return nil, fmt.Errorf("读取复测证据失败: %w", err)
	}
	defer rows.Close()

	out := make([]EvidenceSnapshot, 0, 8)
	for rows.Next() {
		var snap EvidenceSnapshot
		if err := rows.Scan(&snap.TenantID, &snap.ID, &snap.MissionID, &snap.Tool,
			&snap.RequestHash, &snap.RequestSize, &snap.ResponseHash, &snap.ResponseSize,
			&snap.Signature, &snap.Redaction, &snap.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析复测证据失败: %w", err)
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历复测证据失败: %w", err)
	}
	return out, nil
}

// EvidenceSnapshotIDByBlobHash 按 blob 明文哈希反查引用它的证据快照（取最新一条）。
//
// 用途：流量正文读取接口在成功解密后，如果该正文同时被某条证据快照引用，
// 就把这次读取记到对应快照的访问审计上；没有快照引用时返回 (0,false,nil)，
// 表示这次读取不属于任何证据对象。
func (s *Store) EvidenceSnapshotIDByBlobHash(ctx context.Context, tenantID int64, hash string) (int64, bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, false, err
	}
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return 0, false, fmt.Errorf("%w：blob 哈希不能为空", ErrInvalidInput)
	}
	var id int64
	err := s.db.pool.QueryRow(ctx, `
		SELECT id FROM evidence_snapshots
		WHERE tenant_id = $1 AND (request_hash = $2 OR response_hash = $2)
		ORDER BY id DESC LIMIT 1`, tenantID, hash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("按哈希查询证据快照失败: %w", err)
	}
	return id, true, nil
}

// ListEvidenceSnapshotsForResign 返回租户全部证据快照（按 id 升序）。
//
// 证据密钥轮换后签名子密钥变化，需要遍历全部快照用新密钥重算 Signature；
// 这里不做分页：单租户 v1 的证据快照量级可控，且重签必须是完整一遍。
func (s *Store) ListEvidenceSnapshotsForResign(ctx context.Context, tenantID int64) ([]EvidenceSnapshot, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.pool.Query(ctx, `
		SELECT `+`tenant_id, id, mission_id, tool, request_hash, request_size,
		       response_hash, response_size, signature, redaction, created_at
		FROM evidence_snapshots
		WHERE tenant_id = $1
		ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("读取证据快照列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]EvidenceSnapshot, 0, 16)
	for rows.Next() {
		var snap EvidenceSnapshot
		if err := rows.Scan(&snap.TenantID, &snap.ID, &snap.MissionID, &snap.Tool,
			&snap.RequestHash, &snap.RequestSize, &snap.ResponseHash, &snap.ResponseSize,
			&snap.Signature, &snap.Redaction, &snap.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析证据快照失败: %w", err)
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历证据快照失败: %w", err)
	}
	return out, nil
}

// UpdateEvidenceSignature 只更新签名列；轮换重签使用。
//
// 元数据（mission/tool/refs/redaction/created_at）不允许经由此方法修改：签名是
// 对元数据的 HMAC，改元数据必须走 CreateEvidenceSnapshot 重新冻结。
func (s *Store) UpdateEvidenceSignature(ctx context.Context, tenantID, snapshotID int64, signature string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if snapshotID <= 0 {
		return fmt.Errorf("%w：证据标识必须为正数", ErrInvalidInput)
	}
	if strings.TrimSpace(signature) == "" {
		return fmt.Errorf("%w：签名不能为空", ErrInvalidInput)
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE evidence_snapshots SET signature = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, snapshotID, signature)
	if err != nil {
		return fmt.Errorf("更新证据签名失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：证据快照不存在", ErrNotFound)
	}
	return nil
}
