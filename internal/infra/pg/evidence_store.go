package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
)

// 流量采集与证据快照的持久化。

// TrafficExchange 是一次工具调用的采集记录（intent → result 两阶段）。
type TrafficExchange struct {
	TenantID     int64      `json:"tenant_id"`
	ID           int64      `json:"id"`
	MissionID    int64      `json:"mission_id"`
	NodeID       *int64     `json:"node_id,omitempty"`
	Tool         string     `json:"tool"`
	Phase        string     `json:"phase"`
	RequestHash  string     `json:"request_hash"`
	RequestSize  int64      `json:"request_size"`
	ResponseHash string     `json:"response_hash"`
	ResponseSize int64      `json:"response_size"`
	StatusCode   *int       `json:"status_code,omitempty"`
	Error        string     `json:"error"`
	Redaction    string     `json:"redaction"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// EvidenceSnapshot 是冻结的证据版本。
type EvidenceSnapshot struct {
	TenantID     int64     `json:"tenant_id"`
	ID           int64     `json:"id"`
	MissionID    int64     `json:"mission_id"`
	Tool         string    `json:"tool"`
	RequestHash  string    `json:"request_hash"`
	RequestSize  int64     `json:"request_size"`
	ResponseHash string    `json:"response_hash"`
	ResponseSize int64     `json:"response_size"`
	Signature    string    `json:"signature"`
	Redaction    string    `json:"redaction"`
	CreatedAt    time.Time `json:"created_at"`
}

// BeginTraffic 写入请求阶段（intent），返回采集 ID。
func (s *Store) BeginTraffic(ctx context.Context, tenantID, missionID int64, nodeID *int64, tool string, req evidence.BlobRef, redaction string) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.pool.QueryRow(ctx,
		`INSERT INTO traffic_exchanges (tenant_id, mission_id, node_id, tool, phase, request_hash, request_size, redaction)
		 VALUES ($1, $2, $3, $4, 'intent', $5, $6, $7) RETURNING id`,
		tenantID, missionID, nodeID, tool, req.Hash, req.Size, redaction).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写入采集请求失败: %w", err)
	}
	return id, nil
}

// FinishTraffic 写入响应阶段（result）。
func (s *Store) FinishTraffic(ctx context.Context, tenantID, id int64, resp evidence.BlobRef, statusCode int, errText string) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	tag, err := s.db.pool.Exec(ctx,
		`UPDATE traffic_exchanges
		 SET phase = 'result', response_hash = $3, response_size = $4, status_code = $5,
		     error = $6, completed_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND phase = 'intent'`,
		tenantID, id, resp.Hash, resp.Size, statusCode, errText)
	if err != nil {
		return fmt.Errorf("写入采集响应失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("写入采集响应失败: 记录不存在或已完成")
	}
	return nil
}

func scanTraffic(row interface{ Scan(...any) error }) (TrafficExchange, error) {
	var t TrafficExchange
	err := row.Scan(&t.TenantID, &t.ID, &t.MissionID, &t.NodeID, &t.Tool, &t.Phase,
		&t.RequestHash, &t.RequestSize, &t.ResponseHash, &t.ResponseSize,
		&t.StatusCode, &t.Error, &t.Redaction, &t.CreatedAt, &t.CompletedAt)
	if err != nil {
		return TrafficExchange{}, err
	}
	return t, nil
}

const trafficColumns = `tenant_id, id, mission_id, node_id, tool, phase,
	request_hash, request_size, response_hash, response_size,
	status_code, error, redaction, created_at, completed_at`

// ListTraffic 列出任务采集记录。
func (s *Store) ListTraffic(ctx context.Context, tenantID, missionID int64, limit int) ([]TrafficExchange, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.pool.Query(ctx,
		`SELECT `+trafficColumns+` FROM traffic_exchanges
		 WHERE tenant_id = $1 AND mission_id = $2 ORDER BY id DESC LIMIT $3`,
		tenantID, missionID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取采集记录失败: %w", err)
	}
	defer rows.Close()
	out := []TrafficExchange{}
	for rows.Next() {
		t, err := scanTraffic(rows)
		if err != nil {
			return nil, fmt.Errorf("解析采集记录失败: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历采集记录失败: %w", err)
	}
	return out, nil
}

// GetTraffic 读取单条采集记录。
func (s *Store) GetTraffic(ctx context.Context, tenantID, id int64) (TrafficExchange, error) {
	if err := checkTenant(tenantID); err != nil {
		return TrafficExchange{}, err
	}
	t, err := scanTraffic(s.db.pool.QueryRow(ctx,
		`SELECT `+trafficColumns+` FROM traffic_exchanges WHERE tenant_id = $1 AND id = $2`,
		tenantID, id))
	if err != nil {
		return TrafficExchange{}, fmt.Errorf("读取采集记录失败: %w", err)
	}
	return t, nil
}

// CreateEvidenceSnapshot 冻结一条证据快照。
func (s *Store) CreateEvidenceSnapshot(ctx context.Context, tenantID, missionID int64, tool string, req, resp evidence.BlobRef, signature, redaction string, createdAt time.Time) (EvidenceSnapshot, error) {
	if err := checkTenant(tenantID); err != nil {
		return EvidenceSnapshot{}, err
	}
	var snap EvidenceSnapshot
	err := s.db.pool.QueryRow(ctx,
		`INSERT INTO evidence_snapshots
		   (tenant_id, mission_id, tool, request_hash, request_size, response_hash, response_size, signature, redaction, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING tenant_id, id, mission_id, tool, request_hash, request_size,
		           response_hash, response_size, signature, redaction, created_at`,
		tenantID, missionID, tool, req.Hash, req.Size, resp.Hash, resp.Size, signature, redaction, createdAt).
		Scan(&snap.TenantID, &snap.ID, &snap.MissionID, &snap.Tool, &snap.RequestHash, &snap.RequestSize,
			&snap.ResponseHash, &snap.ResponseSize, &snap.Signature, &snap.Redaction, &snap.CreatedAt)
	if err != nil {
		return EvidenceSnapshot{}, fmt.Errorf("创建证据快照失败: %w", err)
	}
	return snap, nil
}

// GetEvidenceSnapshot 读取证据快照。
func (s *Store) GetEvidenceSnapshot(ctx context.Context, tenantID, id int64) (EvidenceSnapshot, error) {
	if err := checkTenant(tenantID); err != nil {
		return EvidenceSnapshot{}, err
	}
	var snap EvidenceSnapshot
	err := s.db.pool.QueryRow(ctx,
		`SELECT tenant_id, id, mission_id, tool, request_hash, request_size,
		        response_hash, response_size, signature, redaction, created_at
		 FROM evidence_snapshots WHERE tenant_id = $1 AND id = $2`, tenantID, id).
		Scan(&snap.TenantID, &snap.ID, &snap.MissionID, &snap.Tool, &snap.RequestHash, &snap.RequestSize,
			&snap.ResponseHash, &snap.ResponseSize, &snap.Signature, &snap.Redaction, &snap.CreatedAt)
	if err != nil {
		return EvidenceSnapshot{}, fmt.Errorf("读取证据快照失败: %w", err)
	}
	return snap, nil
}
