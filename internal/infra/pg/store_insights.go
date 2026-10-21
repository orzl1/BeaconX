package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// M6.2-B3 洞察层存储：全局流量/命令/LLM 记录列表、任务用量、用量聚合与 Dashboard。
//
// 约定与 B1/B2 一致：
//   - 每个方法都显式接收 tenantID，所有 SQL 都带 tenant_id 条件；
//   - 「不存在」统一返回 ErrNotFound，「参数非法」统一返回 ErrInvalidInput；
//   - 返回的切片一律初始化为非 nil，HTTP 层才能稳定输出 [] 而不是 null；
//   - MissionID=0 / Tool="" / Phase="" 等表示「不按该维度筛选」。

const (
	defaultInsightsListLimit = 50
	maxTrafficListLimit      = 200
	maxAuditListLimit        = 200
	maxLLMRecordListLimit    = 500
	// defaultUsageSummaryDays 是 llm-usage/summary 的默认时间窗口（天）。
	defaultUsageSummaryDays = 30
	// maxUsageSummaryDays 是 summary 允许的最大时间窗口（天）。
	maxUsageSummaryDays = 365
)

// dashboardMissionStatuses 是 Dashboard 任务状态分布的固定键（与 0003 的 CHECK 一致）。
var dashboardMissionStatuses = []string{
	"queued", "running", "paused", "finalizing", "done", "failed", "cancelled",
}

// dashboardFindingSeverities 是 Dashboard 发现严重级别分布的固定键。
var dashboardFindingSeverities = []string{"info", "low", "medium", "high", "critical"}

// insightsLimit 把列表 limit 钳制到 [1, max]；<=0 时取默认值。
func insightsLimit(limit, def, max int) int {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	return limit
}

// ListTrafficFilter 按可选条件列出全局流量采集记录（id 倒序）。
//
// missionID=0、tool/phase 为空表示不筛选；phase 只允许 intent/result。
func (s *Store) ListTrafficFilter(ctx context.Context, tenantID int64, f TrafficFilter) ([]TrafficExchange, error) {
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
		t, err := scanTraffic(rows)
		if err != nil {
			return nil, fmt.Errorf("解析流量记录失败: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历流量记录失败: %w", err)
	}
	return out, nil
}

// DeleteTraffic 删除一条流量采集行；不存在返回 ErrNotFound。
//
// 只删采集行：正文 blob 与已冻结的证据快照都保留——快照是审计证据，
// 不能因为清理采集列表而消失（正文按内容寻址，仍可被快照引用）。
func (s *Store) DeleteTraffic(ctx context.Context, tenantID, id int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("%w：采集记录标识必须为正数", ErrInvalidInput)
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM traffic_exchanges WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("删除流量记录失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：流量记录 %d 不存在", ErrNotFound, id)
	}
	return nil
}

// auditEventColumns 是 audit_events 的列（与 policy_store.go 的读取保持一致）。
const auditEventColumns = `tenant_id, id, mission_id, node_id, tool, action, rule_name,
	args_digest, outcome, detail, actor, created_at`

// scanAuditEvent 把一行扫描成 AuditEvent；无行时返回 ErrNotFound。
func scanAuditEvent(row pgx.Row) (AuditEvent, error) {
	var a AuditEvent
	err := row.Scan(&a.TenantID, &a.ID, &a.MissionID, &a.NodeID, &a.Tool, &a.Action,
		&a.RuleName, &a.ArgsDigest, &a.Outcome, &a.Detail, &a.Actor, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuditEvent{}, fmt.Errorf("%w：命令记录不存在", ErrNotFound)
	}
	if err != nil {
		return AuditEvent{}, fmt.Errorf("读取命令记录失败: %w", err)
	}
	return a, nil
}

// GetAuditEvent 读取一条命令执行审计；不存在返回 ErrNotFound。
func (s *Store) GetAuditEvent(ctx context.Context, tenantID, id int64) (AuditEvent, error) {
	if err := checkTenant(tenantID); err != nil {
		return AuditEvent{}, err
	}
	return scanAuditEvent(s.db.pool.QueryRow(ctx,
		`SELECT `+auditEventColumns+` FROM audit_events WHERE tenant_id = $1 AND id = $2`,
		tenantID, id))
}

// ListAuditFilter 按可选条件列出命令执行审计（id 倒序）。
func (s *Store) ListAuditFilter(ctx context.Context, tenantID int64, f AuditFilter) ([]AuditEvent, error) {
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
		a, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历命令记录失败: %w", err)
	}
	return out, nil
}

// llmUsageColumns 是 llm_usage 的列。
const llmUsageColumns = `tenant_id, id, mission_id, role, model,
	prompt_tokens, completion_tokens, total_tokens, created_at`

// scanLLMUsageRecord 把一行扫描成 LLMUsageRecord。
func scanLLMUsageRecord(row pgx.Row) (LLMUsageRecord, error) {
	var r LLMUsageRecord
	if err := row.Scan(&r.TenantID, &r.ID, &r.MissionID, &r.Role, &r.Model,
		&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CreatedAt); err != nil {
		return LLMUsageRecord{}, fmt.Errorf("读取 LLM 用量记录失败: %w", err)
	}
	return r, nil
}

// ListLLMRecords 按可选条件列出模型调用记录（id 倒序）。
func (s *Store) ListLLMRecords(ctx context.Context, tenantID int64, f LLMRecordFilter) ([]LLMUsageRecord, error) {
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
		r, err := scanLLMUsageRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 LLM 记录失败: %w", err)
	}
	return out, nil
}

// MissionUsage 汇总一个任务的预算、全部调用记录与按角色/模型聚合。
// 任务不存在返回 ErrNotFound（调用方通常已先用 GetMission 判 404）。
func (s *Store) MissionUsage(ctx context.Context, tenantID, missionID int64) (MissionUsage, error) {
	if err := checkTenant(tenantID); err != nil {
		return MissionUsage{}, err
	}
	if missionID <= 0 {
		return MissionUsage{}, fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}

	var usage MissionUsage
	err := s.db.pool.QueryRow(ctx,
		`SELECT max_tokens, spent_tokens, max_steps, steps_used
		 FROM missions WHERE tenant_id = $1 AND id = $2`, tenantID, missionID).
		Scan(&usage.MaxTokens, &usage.SpentTokens, &usage.MaxSteps, &usage.StepsUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return MissionUsage{}, fmt.Errorf("%w：任务 %d 不存在", ErrNotFound, missionID)
	}
	if err != nil {
		return MissionUsage{}, fmt.Errorf("读取任务用量失败: %w", err)
	}
	usage.MissionID = missionID

	records, err := s.listMissionLLMRecords(ctx, tenantID, missionID)
	if err != nil {
		return MissionUsage{}, err
	}
	usage.Records = records

	usage.ByRole, err = s.usageByRole(ctx, tenantID, missionID)
	if err != nil {
		return MissionUsage{}, err
	}
	usage.ByModel, err = s.usageByModel(ctx, tenantID, missionID)
	if err != nil {
		return MissionUsage{}, err
	}
	return usage, nil
}

// listMissionLLMRecords 读一个任务的全部调用记录（不分页，单任务量级可控）。
func (s *Store) listMissionLLMRecords(ctx context.Context, tenantID, missionID int64) ([]LLMUsageRecord, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT `+llmUsageColumns+` FROM llm_usage
		 WHERE tenant_id = $1 AND mission_id = $2 ORDER BY id DESC`, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("读取任务调用记录失败: %w", err)
	}
	defer rows.Close()

	out := []LLMUsageRecord{}
	for rows.Next() {
		r, err := scanLLMUsageRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历任务调用记录失败: %w", err)
	}
	return out, nil
}

// usageByRole 按角色聚合任务用量。
func (s *Store) usageByRole(ctx context.Context, tenantID, missionID int64) ([]UsageByRole, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT role, count(*), COALESCE(SUM(total_tokens), 0)
		 FROM llm_usage WHERE tenant_id = $1 AND mission_id = $2
		 GROUP BY role ORDER BY role`, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("按角色聚合用量失败: %w", err)
	}
	defer rows.Close()

	out := []UsageByRole{}
	for rows.Next() {
		var item UsageByRole
		if err := rows.Scan(&item.Role, &item.Calls, &item.TotalTokens); err != nil {
			return nil, fmt.Errorf("解析角色用量失败: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历角色用量失败: %w", err)
	}
	return out, nil
}

// usageByModel 按模型聚合任务用量。
func (s *Store) usageByModel(ctx context.Context, tenantID, missionID int64) ([]UsageByModel, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT model, count(*), COALESCE(SUM(total_tokens), 0)
		 FROM llm_usage WHERE tenant_id = $1 AND mission_id = $2
		 GROUP BY model ORDER BY model`, tenantID, missionID)
	if err != nil {
		return nil, fmt.Errorf("按模型聚合用量失败: %w", err)
	}
	defer rows.Close()

	out := []UsageByModel{}
	for rows.Next() {
		var item UsageByModel
		if err := rows.Scan(&item.Model, &item.Calls, &item.TotalTokens); err != nil {
			return nil, fmt.Errorf("解析模型用量失败: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历模型用量失败: %w", err)
	}
	return out, nil
}

// LLMUsageSummary 按 model/role/day 聚合最近 days 天的模型用量。
//
// missionID=0 表示不按任务筛选；day 分组用 date_trunc('day')，key 形如
// "2026-10-21"。days 超出 [1, 365] 时被钳制，groupBy 非法返回 ErrInvalidInput。
func (s *Store) LLMUsageSummary(ctx context.Context, tenantID int64, groupBy string, missionID int64, days int) ([]LLMUsageGroup, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	var expr string
	switch groupBy {
	case "model":
		expr = "model"
	case "role":
		expr = "role"
	case "day":
		expr = "to_char(date_trunc('day', created_at), 'YYYY-MM-DD')"
	default:
		return nil, fmt.Errorf("%w：group_by 只能是 model、role 或 day", ErrInvalidInput)
	}
	if days <= 0 {
		days = defaultUsageSummaryDays
	}
	if days > maxUsageSummaryDays {
		days = maxUsageSummaryDays
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	conds := []string{"tenant_id = $1", "created_at >= $2"}
	args := []any{tenantID, cutoff}
	if missionID > 0 {
		args = append(args, missionID)
		conds = append(conds, fmt.Sprintf("mission_id = $%d", len(args)))
	}

	q := `SELECT ` + expr + ` AS group_key, count(*),
	             COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
	             COALESCE(SUM(total_tokens), 0)
	      FROM llm_usage WHERE ` + strings.Join(conds, " AND ") +
		` GROUP BY 1 ORDER BY 1`
	rows, err := s.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("聚合 LLM 用量失败: %w", err)
	}
	defer rows.Close()

	out := []LLMUsageGroup{}
	for rows.Next() {
		var g LLMUsageGroup
		if err := rows.Scan(&g.Key, &g.Calls, &g.PromptTokens, &g.CompletionTokens, &g.TotalTokens); err != nil {
			return nil, fmt.Errorf("解析用量分组失败: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用量分组失败: %w", err)
	}
	return out, nil
}

// dashboardCount 执行一条返回单个计数的查询。
func (s *Store) dashboardCount(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := s.db.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Dashboard 汇总首页需要的计数；所有查询都限定当前租户。
//
// 状态/严重级别分布使用固定键（未知值不会出现在 map 里），便于前端直接渲染。
func (s *Store) Dashboard(ctx context.Context, tenantID int64) (DashboardStats, error) {
	if err := checkTenant(tenantID); err != nil {
		return DashboardStats{}, err
	}

	stats := DashboardStats{
		Missions:       DashboardMissions{ByStatus: map[string]int64{}},
		Findings:       DashboardFindings{BySeverity: map[string]int64{}},
		RecentMissions: []DashboardRecentMission{},
	}
	for _, status := range dashboardMissionStatuses {
		stats.Missions.ByStatus[status] = 0
	}
	for _, severity := range dashboardFindingSeverities {
		stats.Findings.BySeverity[severity] = 0
	}

	// 任务总数与状态分布。
	rows, err := s.db.pool.Query(ctx,
		`SELECT status, count(*) FROM missions WHERE tenant_id = $1 GROUP BY status`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("统计任务状态失败: %w", err)
	}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return DashboardStats{}, fmt.Errorf("解析任务状态失败: %w", err)
		}
		stats.Missions.Total += n
		if _, known := stats.Missions.ByStatus[status]; known {
			stats.Missions.ByStatus[status] = n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DashboardStats{}, fmt.Errorf("遍历任务状态失败: %w", err)
	}
	rows.Close()

	// token 口径：spent_total = 任务累计预算消耗；calls = 调用次数；today = 当天用量。
	spentTotal, err := s.dashboardCount(ctx,
		`SELECT COALESCE(SUM(spent_tokens), 0) FROM missions WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("统计 token 消耗失败: %w", err)
	}
	calls, err := s.dashboardCount(ctx,
		`SELECT count(*) FROM llm_usage WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("统计模型调用次数失败: %w", err)
	}
	today, err := s.dashboardCount(ctx,
		`SELECT COALESCE(SUM(total_tokens), 0) FROM llm_usage
		 WHERE tenant_id = $1 AND created_at >= date_trunc('day', now())`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("统计当天 token 用量失败: %w", err)
	}
	stats.Tokens = DashboardTokens{SpentTotal: spentTotal, Calls: calls, Today: today}

	// 待审批数量。
	pending, err := s.dashboardCount(ctx,
		`SELECT count(*) FROM approvals WHERE tenant_id = $1 AND status = 'pending'`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("统计待审批失败: %w", err)
	}
	stats.Approvals.Pending = pending

	// 发现总数与严重级别分布。
	frows, err := s.db.pool.Query(ctx,
		`SELECT severity, count(*) FROM findings WHERE tenant_id = $1 GROUP BY severity`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("统计发现分布失败: %w", err)
	}
	for frows.Next() {
		var severity string
		var n int64
		if err := frows.Scan(&severity, &n); err != nil {
			frows.Close()
			return DashboardStats{}, fmt.Errorf("解析发现分布失败: %w", err)
		}
		stats.Findings.Total += n
		if _, known := stats.Findings.BySeverity[severity]; known {
			stats.Findings.BySeverity[severity] = n
		}
	}
	if err := frows.Err(); err != nil {
		frows.Close()
		return DashboardStats{}, fmt.Errorf("遍历发现分布失败: %w", err)
	}
	frows.Close()

	assets, err := s.dashboardCount(ctx,
		`SELECT count(*) FROM assets WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("统计资产失败: %w", err)
	}
	stats.Assets.Total = assets

	traffic, err := s.dashboardCount(ctx,
		`SELECT count(*) FROM traffic_exchanges WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("统计流量失败: %w", err)
	}
	stats.Traffic.Total = traffic

	// 最近任务（按 id 倒序，最多 5 条）。
	mrows, err := s.db.pool.Query(ctx,
		`SELECT id, name, status, spent_tokens, created_at
		 FROM missions WHERE tenant_id = $1 ORDER BY id DESC LIMIT 5`, tenantID)
	if err != nil {
		return DashboardStats{}, fmt.Errorf("读取最近任务失败: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var m DashboardRecentMission
		if err := mrows.Scan(&m.ID, &m.Name, &m.Status, &m.SpentTokens, &m.CreatedAt); err != nil {
			return DashboardStats{}, fmt.Errorf("解析最近任务失败: %w", err)
		}
		stats.RecentMissions = append(stats.RecentMissions, m)
	}
	if err := mrows.Err(); err != nil {
		return DashboardStats{}, fmt.Errorf("遍历最近任务失败: %w", err)
	}
	return stats, nil
}
