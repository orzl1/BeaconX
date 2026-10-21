package pg

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 本文件实现 B4 的通知渠道与投递记录存储（migration 0009）。
//
// 约定与其它 store 一致：
//   - 每个方法都显式接收 tenantID，所有 SQL 都带 tenant_id 条件；
//   - 「不存在」统一返回 ErrNotFound，「名称冲突」返回 ErrConflict，
//     「请求参数非法」返回 ErrInvalidInput，调用方用 errors.Is 判断；
//   - 返回的切片一律初始化为非 nil，HTTP 层才能稳定输出 [] 而不是 null；
//   - config 是 JSONB，但这里只接受 JSON 对象（map[string]any），避免数组/标量
//     让前端与发送器出现两种解析路径。

// notificationChannelTypes 与 0009 的 CHECK 约束保持一致。
var notificationChannelTypes = map[string]bool{
	"webhook": true,
	"email":   true,
	"generic": true,
}

// notificationDeliveryStatuses 与 0009 的 CHECK 约束保持一致。
var notificationDeliveryStatuses = map[string]bool{
	"pending": true,
	"sent":    true,
	"failed":  true,
	"skipped": true,
}

// NotificationChannel 是一个通知渠道。
type NotificationChannel struct {
	TenantID  int64          `json:"tenant_id"`
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Type      string         `json:"type"` // webhook | email | generic
	Config    map[string]any `json:"config"`
	Enabled   bool           `json:"enabled"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// NotificationChannelInput 是创建渠道的输入。
type NotificationChannelInput struct {
	Name    string
	Type    string
	Config  map[string]any
	Enabled bool
}

// NotificationChannelUpdate 是渠道局部更新输入：nil 指针表示不修改。
//
// Config 与 ClearConfig 互斥：前者用新对象整体替换，后者把配置清空为 {}。
type NotificationChannelUpdate struct {
	Name        *string
	Config      map[string]any
	ClearConfig bool
	Enabled     *bool
}

// NotificationDelivery 是一条投递/测试记录。
type NotificationDelivery struct {
	TenantID  int64     `json:"tenant_id"`
	ID        int64     `json:"id"`
	ChannelID int64     `json:"channel_id"`
	Event     string    `json:"event"`
	Status    string    `json:"status"` // pending | sent | failed | skipped
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

const notificationChannelColumns = `tenant_id, id, name, type, config, enabled, created_at, updated_at`
const notificationDeliveryColumns = `tenant_id, id, channel_id, event, status, detail, created_at`

// validateNotificationChannel 校验渠道字段；config 只对 webhook 做结构校验，
// email/generic 的配置形状由发送器解释（当前版本都只记录 skipped）。
func validateNotificationChannel(name, channelType string, config map[string]any) error {
	if name == "" {
		return fmt.Errorf("%w：渠道名称不能为空", ErrInvalidInput)
	}
	if len(name) > 200 {
		return fmt.Errorf("%w：渠道名称不能超过 200 字节", ErrInvalidInput)
	}
	if !notificationChannelTypes[channelType] {
		return fmt.Errorf("%w：渠道类型必须是 webhook、email 或 generic", ErrInvalidInput)
	}
	if channelType != "webhook" {
		return nil
	}
	rawURL, ok := config["url"]
	if !ok {
		return fmt.Errorf("%w：webhook 渠道缺少 config.url", ErrInvalidInput)
	}
	urlText, ok := rawURL.(string)
	if !ok || strings.TrimSpace(urlText) == "" {
		return fmt.Errorf("%w：webhook 渠道的 config.url 必须是非空字符串", ErrInvalidInput)
	}
	parsed, err := url.Parse(strings.TrimSpace(urlText))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%w：config.url 必须是合法的 http/https 地址", ErrInvalidInput)
	}
	return nil
}

func scanNotificationChannel(row pgx.Row) (NotificationChannel, error) {
	var (
		channel NotificationChannel
		raw     []byte
	)
	err := row.Scan(&channel.TenantID, &channel.ID, &channel.Name, &channel.Type,
		&raw, &channel.Enabled, &channel.CreatedAt, &channel.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationChannel{}, fmt.Errorf("%w：通知渠道不存在", ErrNotFound)
	}
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("读取通知渠道失败: %w", err)
	}
	if channel.Config, err = decodeJSON(raw); err != nil {
		return NotificationChannel{}, err
	}
	return channel, nil
}

func scanNotificationDelivery(row pgx.Row) (NotificationDelivery, error) {
	var delivery NotificationDelivery
	err := row.Scan(&delivery.TenantID, &delivery.ID, &delivery.ChannelID,
		&delivery.Event, &delivery.Status, &delivery.Detail, &delivery.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationDelivery{}, fmt.Errorf("%w：投递记录不存在", ErrNotFound)
	}
	if err != nil {
		return NotificationDelivery{}, fmt.Errorf("读取投递记录失败: %w", err)
	}
	return delivery, nil
}

// isNotificationForeignKeyViolation 判断错误是否为外键冲突（SQLSTATE 23503）。
func isNotificationForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// ListNotificationChannels 按 id 倒序列出渠道；limit 缺省 50、上限 200。
func (s *Store) ListNotificationChannels(ctx context.Context, tenantID int64, limit int) ([]NotificationChannel, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	limit = normalizeLimit(limit, defaultListLimit, maxListLimit)
	rows, err := s.db.pool.Query(ctx,
		`SELECT `+notificationChannelColumns+`
		 FROM notification_channels
		 WHERE tenant_id = $1
		 ORDER BY id DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出通知渠道失败: %w", err)
	}
	defer rows.Close()

	out := make([]NotificationChannel, 0, 8)
	for rows.Next() {
		var (
			channel NotificationChannel
			raw     []byte
		)
		if err := rows.Scan(&channel.TenantID, &channel.ID, &channel.Name, &channel.Type,
			&raw, &channel.Enabled, &channel.CreatedAt, &channel.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析通知渠道失败: %w", err)
		}
		if channel.Config, err = decodeJSON(raw); err != nil {
			return nil, err
		}
		out = append(out, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历通知渠道失败: %w", err)
	}
	return out, nil
}

// GetNotificationChannel 按 ID 读取渠道。
func (s *Store) GetNotificationChannel(ctx context.Context, tenantID, channelID int64) (NotificationChannel, error) {
	if err := checkTenant(tenantID); err != nil {
		return NotificationChannel{}, err
	}
	if channelID <= 0 {
		return NotificationChannel{}, fmt.Errorf("%w：渠道标识必须为正数", ErrInvalidInput)
	}
	return scanNotificationChannel(s.db.pool.QueryRow(ctx,
		`SELECT `+notificationChannelColumns+`
		 FROM notification_channels WHERE tenant_id = $1 AND id = $2`, tenantID, channelID))
}

// CreateNotificationChannel 创建渠道；同租户内 name 重复返回 ErrConflict。
func (s *Store) CreateNotificationChannel(ctx context.Context, tenantID int64, in NotificationChannelInput) (NotificationChannel, error) {
	if err := checkTenant(tenantID); err != nil {
		return NotificationChannel{}, err
	}
	name := strings.TrimSpace(in.Name)
	channelType := strings.TrimSpace(in.Type)
	config := in.Config
	if config == nil {
		config = map[string]any{}
	}
	if err := validateNotificationChannel(name, channelType, config); err != nil {
		return NotificationChannel{}, err
	}
	rawConfig, err := marshalJSON(config)
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("%w：config 不是合法的 JSON 对象: %v", ErrInvalidInput, err)
	}

	channel, err := scanNotificationChannel(s.db.pool.QueryRow(ctx, `
		INSERT INTO notification_channels (tenant_id, name, type, config, enabled)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+notificationChannelColumns,
		tenantID, name, channelType, rawConfig, in.Enabled))
	if isUniqueViolation(err) {
		return NotificationChannel{}, fmt.Errorf("%w：渠道名称 %q 已存在", ErrConflict, name)
	}
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("创建通知渠道失败: %w", err)
	}
	return channel, nil
}

// UpdateNotificationChannel 局部更新渠道（name/config/enabled）。
//
// 类型不可改：不同发送器的配置形状不一样，改类型只会让存量 config 变得不可解释。
// 配置结构按「更新后的字段」整体校验，避免出现合法类型配非法配置。
func (s *Store) UpdateNotificationChannel(ctx context.Context, tenantID, channelID int64, in NotificationChannelUpdate) (NotificationChannel, error) {
	if err := checkTenant(tenantID); err != nil {
		return NotificationChannel{}, err
	}
	if channelID <= 0 {
		return NotificationChannel{}, fmt.Errorf("%w：渠道标识必须为正数", ErrInvalidInput)
	}
	if in.Config != nil && in.ClearConfig {
		return NotificationChannel{}, fmt.Errorf("%w：config 与清空配置不能同时提供", ErrInvalidInput)
	}
	current, err := s.GetNotificationChannel(ctx, tenantID, channelID)
	if err != nil {
		return NotificationChannel{}, err
	}

	name := current.Name
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	config := current.Config
	if in.ClearConfig {
		config = map[string]any{}
	}
	if in.Config != nil {
		config = in.Config
	}
	enabled := current.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if err := validateNotificationChannel(name, current.Type, config); err != nil {
		return NotificationChannel{}, err
	}
	rawConfig, err := marshalJSON(config)
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("%w：config 不是合法的 JSON 对象: %v", ErrInvalidInput, err)
	}

	channel, err := scanNotificationChannel(s.db.pool.QueryRow(ctx, `
		UPDATE notification_channels
		SET name = $3, config = $4, enabled = $5
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+notificationChannelColumns,
		tenantID, channelID, name, rawConfig, enabled))
	if isUniqueViolation(err) {
		return NotificationChannel{}, fmt.Errorf("%w：渠道名称 %q 已存在", ErrConflict, name)
	}
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("更新通知渠道失败: %w", err)
	}
	return channel, nil
}

// DeleteNotificationChannel 删除渠道；其投递记录由外键级联删除。
func (s *Store) DeleteNotificationChannel(ctx context.Context, tenantID, channelID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if channelID <= 0 {
		return fmt.Errorf("%w：渠道标识必须为正数", ErrInvalidInput)
	}
	tag, err := s.db.pool.Exec(ctx,
		`DELETE FROM notification_channels WHERE tenant_id = $1 AND id = $2`, tenantID, channelID)
	if err != nil {
		return fmt.Errorf("删除通知渠道失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w：通知渠道不存在", ErrNotFound)
	}
	return nil
}

// CreateNotificationDelivery 追加一条投递记录。
//
// 渠道不存在（外键冲突）返回 ErrNotFound；status 非法返回 ErrInvalidInput。
func (s *Store) CreateNotificationDelivery(ctx context.Context, tenantID, channelID int64, event, status, detail string) (NotificationDelivery, error) {
	if err := checkTenant(tenantID); err != nil {
		return NotificationDelivery{}, err
	}
	if channelID <= 0 {
		return NotificationDelivery{}, fmt.Errorf("%w：渠道标识必须为正数", ErrInvalidInput)
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if !notificationDeliveryStatuses[status] {
		return NotificationDelivery{}, fmt.Errorf("%w：投递状态必须是 pending/sent/failed/skipped", ErrInvalidInput)
	}
	delivery, err := scanNotificationDelivery(s.db.pool.QueryRow(ctx, `
		INSERT INTO notification_deliveries (tenant_id, channel_id, event, status, detail)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+notificationDeliveryColumns,
		tenantID, channelID, strings.TrimSpace(event), status, detail))
	if isNotificationForeignKeyViolation(err) {
		return NotificationDelivery{}, fmt.Errorf("%w：通知渠道不存在", ErrNotFound)
	}
	if err != nil {
		return NotificationDelivery{}, fmt.Errorf("写入投递记录失败: %w", err)
	}
	return delivery, nil
}

// ListNotificationDeliveries 按投递 id 倒序列出记录。
//
// channelID=0 表示不按渠道筛选；status 为空表示不按状态筛选。
func (s *Store) ListNotificationDeliveries(ctx context.Context, tenantID, channelID int64, status string, limit int) ([]NotificationDelivery, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	if channelID < 0 {
		return nil, fmt.Errorf("%w：渠道标识不能为负数", ErrInvalidInput)
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "" && !notificationDeliveryStatuses[status] {
		return nil, fmt.Errorf("%w：投递状态必须是 pending/sent/failed/skipped", ErrInvalidInput)
	}
	limit = normalizeLimit(limit, defaultListLimit, maxListLimit)

	conds := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if channelID > 0 {
		args = append(args, channelID)
		conds = append(conds, fmt.Sprintf("channel_id = $%d", len(args)))
	}
	if status != "" {
		args = append(args, status)
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	args = append(args, limit)
	query := `SELECT ` + notificationDeliveryColumns + ` FROM notification_deliveries WHERE ` +
		strings.Join(conds, " AND ") + fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := s.db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("列出投递记录失败: %w", err)
	}
	defer rows.Close()

	out := make([]NotificationDelivery, 0, 8)
	for rows.Next() {
		var delivery NotificationDelivery
		if err := rows.Scan(&delivery.TenantID, &delivery.ID, &delivery.ChannelID,
			&delivery.Event, &delivery.Status, &delivery.Detail, &delivery.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析投递记录失败: %w", err)
		}
		out = append(out, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历投递记录失败: %w", err)
	}
	return out, nil
}

// ---------- 事件推送（B8） ----------

// NotificationEvent 是「某事件已投递给某渠道」的幂等台账行。
type NotificationEvent struct {
	TenantID   int64     `json:"tenant_id"`
	ID         int64     `json:"id"`
	EventID    int64     `json:"event_id"`
	ChannelID  int64     `json:"channel_id"`
	DeliveryID *int64    `json:"delivery_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// MaxMissionEventID 返回当前最大任务事件 id（dispatcher 首次启动的起点）。
func (s *Store) MaxMissionEventID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.pool.QueryRow(ctx, `SELECT COALESCE(MAX(id), 0) FROM mission_events`).Scan(&id); err != nil {
		return 0, fmt.Errorf("读取任务事件游标失败: %w", err)
	}
	return id, nil
}

// LatestNotificationEventCursor 返回已处理的最大事件 id；没有台账时返回 0。
func (s *Store) LatestNotificationEventCursor(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(event_id), 0) FROM notification_events`).Scan(&id); err != nil {
		return 0, fmt.Errorf("读取通知事件游标失败: %w", err)
	}
	return id, nil
}

// ListMissionEventsAfter 跨租户按 id 升序读取事件（dispatcher 专用只读查询）。
//
// mission_events.id 是全局自增且跨租户唯一，因此可以只按 id 游标扫描；每行携带
// tenant_id 供 dispatcher 选择该租户的渠道。limit 缺省 100、上限 500。
func (s *Store) ListMissionEventsAfter(ctx context.Context, afterID int64, limit int) ([]MissionEvent, error) {
	if afterID < 0 {
		return nil, fmt.Errorf("%w：事件游标不能为负数", ErrInvalidInput)
	}
	limit = normalizeLimit(limit, 100, 500)
	rows, err := s.db.pool.Query(ctx, `
		SELECT tenant_id, id, mission_id, type, actor, reason, payload, created_at
		FROM mission_events
		WHERE id > $1
		ORDER BY id ASC LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取事件推送队列失败: %w", err)
	}
	defer rows.Close()

	out := make([]MissionEvent, 0, limit)
	for rows.Next() {
		var (
			event   MissionEvent
			payload []byte
		)
		if err := rows.Scan(&event.TenantID, &event.ID, &event.MissionID, &event.Type,
			&event.Actor, &event.Reason, &payload, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("解析事件推送队列失败: %w", err)
		}
		if event.Payload, err = decodeJSON(payload); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历事件推送队列失败: %w", err)
	}
	return out, nil
}

// ListEnabledNotificationChannelsForPush 返回租户启用中的全部渠道（dispatcher 用）。
func (s *Store) ListEnabledNotificationChannelsForPush(ctx context.Context, tenantID int64) ([]NotificationChannel, error) {
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.pool.Query(ctx,
		`SELECT `+notificationChannelColumns+`
		 FROM notification_channels
		 WHERE tenant_id = $1 AND enabled = true
		 ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("读取推送渠道失败: %w", err)
	}
	defer rows.Close()

	out := make([]NotificationChannel, 0, 8)
	for rows.Next() {
		var (
			channel NotificationChannel
			raw     []byte
		)
		if err := rows.Scan(&channel.TenantID, &channel.ID, &channel.Name, &channel.Type,
			&raw, &channel.Enabled, &channel.CreatedAt, &channel.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析推送渠道失败: %w", err)
		}
		if channel.Config, err = decodeJSON(raw); err != nil {
			return nil, err
		}
		out = append(out, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历推送渠道失败: %w", err)
	}
	return out, nil
}

// BeginNotificationEvent 抢占「事件+渠道」的投递权；已存在时返回 fresh=false。
//
// 先占位再发送：并发 dispatcher/多副本下只有一个调用方会真正发送，实现
// at-most-once。发送失败也保留台账（不重试），失败原因写入投递记录。
func (s *Store) BeginNotificationEvent(ctx context.Context, tenantID, eventID, channelID int64) (int64, bool, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, false, err
	}
	if eventID <= 0 || channelID <= 0 {
		return 0, false, fmt.Errorf("%w：事件与渠道标识必须为正数", ErrInvalidInput)
	}
	var id int64
	err := s.db.pool.QueryRow(ctx, `
		INSERT INTO notification_events (tenant_id, event_id, channel_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, event_id, channel_id) DO NOTHING
		RETURNING id`, tenantID, eventID, channelID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if isNotificationForeignKeyViolation(err) {
		// 渠道在读取与占位之间被删除：按「无需投递」处理，而不是记失败。
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("写入通知事件台账失败: %w", err)
	}
	return id, true, nil
}

// CompleteNotificationEvent 回填投递记录 id（best-effort，失败不影响发送结论）。
func (s *Store) CompleteNotificationEvent(ctx context.Context, tenantID, notificationEventID, deliveryID int64) error {
	if err := checkTenant(tenantID); err != nil {
		return err
	}
	if notificationEventID <= 0 || deliveryID <= 0 {
		return fmt.Errorf("%w：通知事件与投递标识必须为正数", ErrInvalidInput)
	}
	_, err := s.db.pool.Exec(ctx, `
		UPDATE notification_events SET delivery_id = $3
		WHERE tenant_id = $1 AND id = $2`, tenantID, notificationEventID, deliveryID)
	if err != nil {
		return fmt.Errorf("回填通知事件投递标识失败: %w", err)
	}
	return nil
}
