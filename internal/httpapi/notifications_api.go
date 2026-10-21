package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B4：通知渠道与投递记录接口（P-B4-11..17）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册，NewHandler 构造时用
// Build(Options) 注入 Store/TenantID。语义约定：
//   - 渠道类型：webhook（真实 POST）、email（SMTP 配置，当前不实际发信）、
//     generic（占位，不联网）；
//   - 测试发送：webhook 真实请求并把结果（sent/failed）写入投递记录；
//     email/generic 不联网，写入 skipped 记录；
//   - 测试发送即使失败也返回 200 + delivery，便于前端展示失败原因；只有
//     渠道不存在/参数非法/存储未就绪才返回 4xx/5xx。

const (
	// notificationTestTimeout 是 webhook 测试发送的端到端超时。
	notificationTestTimeout = 5 * time.Second
	// notificationTestBodyLimit 是读取 webhook 响应正文的上限（只用于 detail）。
	notificationTestBodyLimit = 512
)

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/notifications/channels",
			Summary: "通知渠道列表（?limit=）",
			Build:   func(o Options) http.HandlerFunc { return o.handleListNotificationChannels },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/notifications/channels",
			Summary: "新建通知渠道",
			Build:   func(o Options) http.HandlerFunc { return o.handleCreateNotificationChannel },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/notifications/channels/{id}",
			Summary: "通知渠道详情",
			Build:   func(o Options) http.HandlerFunc { return o.handleGetNotificationChannel },
		},
		FeatureRoute{
			Method: http.MethodPatch, Pattern: "/api/v1/notifications/channels/{id}",
			Summary: "更新通知渠道（name/config/enabled）",
			Build:   func(o Options) http.HandlerFunc { return o.handleUpdateNotificationChannel },
		},
		FeatureRoute{
			Method: http.MethodDelete, Pattern: "/api/v1/notifications/channels/{id}",
			Summary: "删除通知渠道",
			Build:   func(o Options) http.HandlerFunc { return o.handleDeleteNotificationChannel },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/notifications/channels/{id}/test",
			Summary: "发送测试消息并记录投递",
			Build:   func(o Options) http.HandlerFunc { return o.handleTestNotificationChannel },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/notifications/deliveries",
			Summary: "通知投递记录（?channel_id=&status=&limit=）",
			Build:   func(o Options) http.HandlerFunc { return o.handleListNotificationDeliveries },
		},
	)
}

type notificationChannelCreateRequest struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Config  map[string]any `json:"config"`
	Enabled *bool          `json:"enabled"`
}

// notificationChannelPatchRequest 是渠道局部更新请求。
//
// config 用 json.RawMessage 承载三态：缺省=不变；JSON null=清空为 {}；
// 对象=整体替换。数组/标量会被拒绝（config 只接受 JSON 对象）。
type notificationChannelPatchRequest struct {
	Name    *string         `json:"name"`
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
}

// notificationChannelPathID 解析路径中的渠道标识。
func (o Options) notificationChannelPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "通知渠道")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}

// handleListNotificationChannels 列出渠道（id 倒序）。
func (o Options) handleListNotificationChannels(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	channels, err := o.Store.ListNotificationChannels(r.Context(), o.TenantID, listLimitParam(r))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if channels == nil {
		channels = []pg.NotificationChannel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
}

// handleCreateNotificationChannel 新建渠道；名称重复返回 409。
func (o Options) handleCreateNotificationChannel(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req notificationChannelCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	channel, err := o.Store.CreateNotificationChannel(r.Context(), o.TenantID, pg.NotificationChannelInput{
		Name:    req.Name,
		Type:    req.Type,
		Config:  req.Config,
		Enabled: enabled,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, channel)
}

// handleGetNotificationChannel 读取渠道详情。
func (o Options) handleGetNotificationChannel(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	channelID, ok := o.notificationChannelPathID(w, r)
	if !ok {
		return
	}
	channel, err := o.Store.GetNotificationChannel(r.Context(), o.TenantID, channelID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, channel)
}

// handleUpdateNotificationChannel 局部更新渠道；类型不可改。
func (o Options) handleUpdateNotificationChannel(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	channelID, ok := o.notificationChannelPathID(w, r)
	if !ok {
		return
	}
	var req notificationChannelPatchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	update := pg.NotificationChannelUpdate{Name: req.Name, Enabled: req.Enabled}
	if len(req.Config) > 0 {
		raw := bytes.TrimSpace(req.Config)
		if bytes.Equal(raw, []byte("null")) {
			update.ClearConfig = true
		} else {
			var config map[string]any
			if err := json.Unmarshal(raw, &config); err != nil || config == nil {
				writeError(w, r, http.StatusBadRequest, "bad_request", "config 必须是 JSON 对象或 null")
				return
			}
			update.Config = config
		}
	}
	channel, err := o.Store.UpdateNotificationChannel(r.Context(), o.TenantID, channelID, update)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, channel)
}

// handleDeleteNotificationChannel 删除渠道（投递记录级联删除）。
func (o Options) handleDeleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	channelID, ok := o.notificationChannelPathID(w, r)
	if !ok {
		return
	}
	if err := o.Store.DeleteNotificationChannel(r.Context(), o.TenantID, channelID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestNotificationChannel 发送测试消息并落一条投递记录。
//
// 响应始终包含 delivery 对象；sent=true 仅表示 webhook 实际发送成功，
// email/generic 的 skipped 与 webhook 的 failed 都返回 200（记录可查）。
func (o Options) handleTestNotificationChannel(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	channelID, ok := o.notificationChannelPathID(w, r)
	if !ok {
		return
	}
	channel, err := o.Store.GetNotificationChannel(r.Context(), o.TenantID, channelID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	status, detail := o.sendNotificationTest(r.Context(), channel)
	delivery, err := o.Store.CreateNotificationDelivery(r.Context(), o.TenantID, channel.ID, "test", status, detail)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sent":       status == "sent",
		"channel_id": channel.ID,
		"delivery":   delivery,
	})
}

// handleListNotificationDeliveries 列出投递记录，可按渠道与状态过滤。
func (o Options) handleListNotificationDeliveries(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	q := r.URL.Query()
	channelID := int64(0)
	if raw := strings.TrimSpace(q.Get("channel_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "channel_id 必须是正整数")
			return
		}
		channelID = parsed
	}
	deliveries, err := o.Store.ListNotificationDeliveries(r.Context(), o.TenantID, channelID, q.Get("status"), listLimitParam(r))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if deliveries == nil {
		deliveries = []pg.NotificationDelivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries})
}

// sendNotificationTest 执行一次测试发送，返回投递状态（sent/failed/skipped）与说明。
//
// webhook 真实 POST JSON {"event":"test","channel":{...}}；其它类型不联网，
// 直接标记 skipped。任何网络/HTTP 非 2xx 都记为 failed，而不是让请求失败，
// 这样投递台账始终能解释「这次测试发生了什么」。
func (o Options) sendNotificationTest(ctx context.Context, channel pg.NotificationChannel) (status, detail string) {
	if channel.Type != "webhook" {
		return "skipped", fmt.Sprintf("%s 渠道不实际发送，测试记录为 skipped", channel.Type)
	}
	urlText, _ := channel.Config["url"].(string)
	payload, err := json.Marshal(map[string]any{
		"event": "test",
		"channel": map[string]any{
			"id":   channel.ID,
			"name": channel.Name,
			"type": channel.Type,
		},
	})
	if err != nil {
		return "failed", "构造测试消息失败: " + err.Error()
	}

	sendCtx, cancel := context.WithTimeout(ctx, notificationTestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, urlText, bytes.NewReader(payload))
	if err != nil {
		return "failed", "构造测试请求失败: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "BeaconX-Notification/1.0")

	client := &http.Client{Timeout: notificationTestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "failed", "发送测试请求失败: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, notificationTestBodyLimit))

	detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		detail += "：" + trimmed
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "sent", detail
	}
	return "failed", detail
}
