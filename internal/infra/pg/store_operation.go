package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/domain/graph"
)

// AddNode 在指定任务中创建作战图节点。
func (s *Store) AddNode(ctx context.Context, tenantID, missionID int64, in NodeInput) (Node, error) {
	if err := checkTenant(tenantID); err != nil {
		return Node{}, err
	}
	if missionID <= 0 {
		return Node{}, fmt.Errorf("%w: 任务标识必须为正数", ErrInvalidInput)
	}
	if err := graph.ValidateNode(in.Kind, in.State); err != nil {
		return Node{}, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	if strings.TrimSpace(in.Title) == "" {
		return Node{}, fmt.Errorf("%w: 节点标题不能为空", ErrInvalidInput)
	}
	payload, err := marshalJSON(in.Payload)
	if err != nil {
		return Node{}, err
	}

	const q = `
		INSERT INTO operation_nodes (tenant_id, mission_id, kind, state, title, body, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING tenant_id, id, mission_id, kind, state, title, body, payload, created_at, updated_at`
	var (
		n      Node
		kind   string
		state  string
		rawPay []byte
	)
	err = s.db.pool.QueryRow(ctx, q, tenantID, missionID, string(in.Kind), string(in.State), in.Title, in.Body, string(payload)).
		Scan(&n.TenantID, &n.ID, &n.MissionID, &kind, &state, &n.Title, &n.Body, &rawPay, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return Node{}, fmt.Errorf("创建作战图节点失败: %w", err)
	}
	n.Kind = graph.NodeKind(kind)
	n.State = graph.NodeState(state)
	if n.Payload, err = decodeJSON(rawPay); err != nil {
		return Node{}, err
	}
	return n, nil
}

// GetNode 按 ID 读取节点。
func (s *Store) GetNode(ctx context.Context, tenantID, nodeID int64) (Node, error) {
	if err := checkTenant(tenantID); err != nil {
		return Node{}, err
	}
	const q = `
		SELECT tenant_id, id, mission_id, kind, state, title, body, payload, created_at, updated_at
		FROM operation_nodes WHERE tenant_id = $1 AND id = $2`
	var (
		n      Node
		kind   string
		state  string
		rawPay []byte
	)
	err := s.db.pool.QueryRow(ctx, q, tenantID, nodeID).
		Scan(&n.TenantID, &n.ID, &n.MissionID, &kind, &state, &n.Title, &n.Body, &rawPay, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return Node{}, fmt.Errorf("读取作战图节点失败: %w", err)
	}
	n.Kind = graph.NodeKind(kind)
	n.State = graph.NodeState(state)
	if n.Payload, err = decodeJSON(rawPay); err != nil {
		return Node{}, err
	}
	return n, nil
}

// SetNodeState 更新节点状态。
//
// 更新前会重新读取节点类型并复跑 graph.ValidateNode：只校验 state 取值是不够的，
// 否则可以把 fact/hint 等知识类节点改成 running，绕过 AddNode 的领域规则。
func (s *Store) SetNodeState(ctx context.Context, tenantID, nodeID int64, state graph.NodeState) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if !state.Valid() {
		return fmt.Errorf("%w: 非法节点状态 %q", ErrInvalidInput, state)
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind string
	err = tx.QueryRow(ctx,
		`SELECT kind FROM operation_nodes WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, nodeID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("更新节点状态失败: 节点不存在")
	}
	if err != nil {
		return fmt.Errorf("读取节点类型失败: %w", err)
	}
	if err := graph.ValidateNode(graph.NodeKind(kind), state); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}

	if _, err := tx.Exec(ctx,
		`UPDATE operation_nodes SET state = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, nodeID, string(state)); err != nil {
		return fmt.Errorf("更新节点状态失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交节点状态失败: %w", err)
	}
	return nil
}

// ListNodes 列出任务节点（按创建顺序）。
func (s *Store) ListNodes(ctx context.Context, tenantID, missionID int64, limit int) ([]Node, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, mission_id, kind, state, title, body, payload, created_at, updated_at
		FROM operation_nodes
		WHERE tenant_id = $1 AND mission_id = $2
		ORDER BY id LIMIT $3`, tenantID, missionID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出节点失败: %w", err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		var (
			n      Node
			kind   string
			state  string
			rawPay []byte
		)
		if err := rows.Scan(&n.TenantID, &n.ID, &n.MissionID, &kind, &state, &n.Title, &n.Body, &rawPay, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析节点失败: %w", err)
		}
		n.Kind = graph.NodeKind(kind)
		n.State = graph.NodeState(state)
		if n.Payload, err = decodeJSON(rawPay); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历节点失败: %w", err)
	}
	return out, nil
}

// AddEdge 创建作战图边。数据库复合外键保证两端属于同一任务。
func (s *Store) AddEdge(ctx context.Context, tenantID, missionID, srcID, dstID int64, rel graph.EdgeRel) (Edge, error) {
	if err := checkTenant(tenantID); err != nil {
		return Edge{}, err
	}
	if err := graph.ValidateEdge(rel); err != nil {
		return Edge{}, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	const q = `
		INSERT INTO operation_edges (tenant_id, mission_id, src_id, dst_id, rel)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING tenant_id, id, mission_id, src_id, dst_id, rel, created_at`
	var (
		e       Edge
		relText string
	)
	err := s.db.pool.QueryRow(ctx, q, tenantID, missionID, srcID, dstID, string(rel)).
		Scan(&e.TenantID, &e.ID, &e.MissionID, &e.SrcID, &e.DstID, &relText, &e.CreatedAt)
	if err != nil {
		return Edge{}, fmt.Errorf("创建作战图边失败: %w", err)
	}
	e.Rel = graph.EdgeRel(relText)
	return e, nil
}

// ListEdges 列出任务边。
func (s *Store) ListEdges(ctx context.Context, tenantID, missionID int64, limit int) ([]Edge, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, mission_id, src_id, dst_id, rel, created_at
		FROM operation_edges
		WHERE tenant_id = $1 AND mission_id = $2
		ORDER BY id LIMIT $3`, tenantID, missionID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出边失败: %w", err)
	}
	defer rows.Close()

	var out []Edge
	for rows.Next() {
		var (
			e   Edge
			rel string
		)
		if err := rows.Scan(&e.TenantID, &e.ID, &e.MissionID, &e.SrcID, &e.DstID, &rel, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析边失败: %w", err)
		}
		e.Rel = graph.EdgeRel(rel)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历边失败: %w", err)
	}
	return out, nil
}

// AddAnchor 把节点锚定到资产；重复锚定同一角色是幂等的。
func (s *Store) AddAnchor(ctx context.Context, tenantID, nodeID, assetID int64, role graph.AnchorRole) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if err := graph.ValidateAnchor(role); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	_, err := s.db.pool.Exec(ctx,
		`INSERT INTO operation_anchors (tenant_id, node_id, asset_id, role)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, node_id, asset_id, role) DO NOTHING`,
		tenantID, nodeID, assetID, string(role))
	if err != nil {
		return fmt.Errorf("创建锚点失败: %w", err)
	}
	return nil
}

// ListAnchors 列出任务的锚点（通过节点归属过滤）。
func (s *Store) ListAnchors(ctx context.Context, tenantID, missionID int64) ([]Anchor, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.pool.Query(ctx, `
		SELECT a.tenant_id, a.node_id, a.asset_id, a.role, a.created_at
		FROM operation_anchors a
		JOIN operation_nodes n ON n.tenant_id = a.tenant_id AND n.id = a.node_id
		WHERE a.tenant_id = $1 AND n.mission_id = $2
		ORDER BY a.node_id, a.asset_id, a.role`, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("列出锚点失败: %w", err)
	}
	defer rows.Close()

	var out []Anchor
	for rows.Next() {
		var (
			a    Anchor
			role string
		)
		if err := rows.Scan(&a.TenantID, &a.NodeID, &a.AssetID, &role, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析锚点失败: %w", err)
		}
		a.Role = graph.AnchorRole(role)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历锚点失败: %w", err)
	}
	return out, nil
}

// Coverage 计算任务资产覆盖度。
//
// 分母 = 任务已登记资产 ∪ 本任务 role='target' 锚定的资产；
// 分子 = 本任务 kind='fact' 节点上 role='tested' 锚定的资产。
// hint/touched 锚点不进入分母，因此挂提示不会拉低覆盖度。
func (s *Store) Coverage(ctx context.Context, tenantID, missionID int64) (Coverage, error) {
	if err := checkTenant(tenantID); err != nil {
		return Coverage{}, err
	}
	const q = `
		WITH denom AS (
		    SELECT asset_id FROM mission_asset_links
		    WHERE tenant_id = $1 AND mission_id = $2
		    UNION
		    SELECT a.asset_id
		    FROM operation_anchors a
		    JOIN operation_nodes n ON n.tenant_id = a.tenant_id AND n.id = a.node_id
		    WHERE a.tenant_id = $1 AND n.mission_id = $2 AND a.role = 'target'
		),
		tested AS (
		    SELECT DISTINCT a.asset_id
		    FROM operation_anchors a
		    JOIN operation_nodes n ON n.tenant_id = a.tenant_id AND n.id = a.node_id
		    WHERE a.tenant_id = $1 AND n.mission_id = $2
		      AND a.role = 'tested' AND n.kind = 'fact'
		)
		SELECT ast.type, COUNT(*) AS total,
		       COUNT(*) FILTER (WHERE t.asset_id IS NOT NULL) AS tested
		FROM denom d
		JOIN assets ast ON ast.tenant_id = $1 AND ast.id = d.asset_id
		LEFT JOIN tested t ON t.asset_id = d.asset_id
		GROUP BY ast.type
		ORDER BY ast.type`
	rows, err := s.db.pool.Query(ctx, q, tenantID, missionID)
	if err != nil {
		return Coverage{}, fmt.Errorf("计算覆盖度失败: %w", err)
	}
	defer rows.Close()

	cov := Coverage{ByType: []TypeCoverage{}}
	for rows.Next() {
		var (
			typ           string
			total, tested int
		)
		if err := rows.Scan(&typ, &total, &tested); err != nil {
			return Coverage{}, fmt.Errorf("解析覆盖度失败: %w", err)
		}
		item := TypeCoverage{
			Type:    asset.Kind(typ),
			Total:   total,
			Tested:  tested,
			Percent: percent(tested, total),
		}
		cov.ByType = append(cov.ByType, item)
		cov.Total += total
		cov.Tested += tested
	}
	if err := rows.Err(); err != nil {
		return Coverage{}, fmt.Errorf("遍历覆盖度失败: %w", err)
	}
	cov.Percent = percent(cov.Tested, cov.Total)
	return cov, nil
}
