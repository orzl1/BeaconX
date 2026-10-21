package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/orzl1/BeaconX/internal/app/evidence"
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
	// notifyAllowPrivateEnv 控制是否允许 webhook 访问回环/私网（默认 false）。
	notifyAllowPrivateEnv = "BEACONX_NOTIFY_ALLOW_PRIVATE"
)

// notifyErrPrivateAddress 表示 webhook 目标解析到回环/私网地址且未放行。
var notifyErrPrivateAddress = errors.New("目标地址属于回环或内网，已拒绝访问")

// notifyBlockedCIDRs 是 net.IP 判定之外的保留/特殊地址段（与 syncdata 的 SSRF
// 策略保持一致，复制最小实现，避免跨任务修改 syncdata 包）。
var notifyBlockedCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"100.64.0.0/10", // CGNAT
		"192.0.0.0/24",  // IETF 协议分配
		"198.18.0.0/15", // 基准测试保留
		"240.0.0.0/4",   // 保留（含广播）
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		if _, network, err := net.ParseCIDR(raw); err == nil {
			out = append(out, network)
		}
	}
	return out
}()

// notifyAllowPrivate 读取 SSRF 放行开关（默认拒绝私网）。
func notifyAllowPrivate() bool {
	raw := strings.TrimSpace(os.Getenv(notifyAllowPrivateEnv))
	if raw == "" {
		return false
	}
	enabled, err := strconv.ParseBool(raw)
	return err == nil && enabled
}

// notifyIPDisallowed 判断 IP 是否属于禁止访问的网段。
func notifyIPDisallowed(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, network := range notifyBlockedCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// notifyValidateWebhookURL 校验 webhook 目标：仅 http/https，且解析出的全部 IP
// 都不是回环/私网/link-local/CGNAT（allowPrivate=false 时）。
func notifyValidateWebhookURL(ctx context.Context, rawURL string, allowPrivate bool) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return errors.New("webhook url 不合法")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("webhook url 只支持 http/https")
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && notifyIPDisallowed(ip) {
			return notifyErrPrivateAddress
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return errors.New("解析 webhook 目标主机失败")
	}
	for _, item := range ips {
		if !allowPrivate && notifyIPDisallowed(item.IP) {
			return notifyErrPrivateAddress
		}
	}
	return nil
}

// notifyHTTPClient 构造带 SSRF 防护的 webhook 客户端：
//   - 拨号前校验真正要连的 IP（DNS 重绑定无法绕过）；
//   - 不跟随重定向（重定向可绕回内网）；
//   - 不走环境代理（HTTP_PROXY 可能把请求带进内网）。
func notifyHTTPClient(allowPrivate bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, errors.New("解析 webhook 目标地址失败")
			}
			if err := notifyValidateWebhookURL(ctx, "http://"+net.JoinHostPort(host, port), allowPrivate); err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("webhook 不跟随 HTTP 重定向")
		},
	}
}

// notifyRedactURL 掩码 URL 里的 userinfo（user:pass@ → ***@），避免凭据出现在
// 响应与投递详情里。
func notifyRedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	// 不用 url.User("***")：它会百分号编码成 %2A%2A%2A，前端可读性差；
	// 这里直接拼出 "scheme://***@host" 的形式。
	parsed.User = nil
	prefix := parsed.Scheme + "://"
	rest := parsed.String()
	hostPart := parsed.Host
	if hostPart == "" || !strings.HasPrefix(rest, prefix+hostPart) {
		return raw
	}
	return strings.Replace(rest, prefix+hostPart, prefix+"***@"+hostPart, 1)
}

// notifySanitizeError 清理错误文本：掩码 URL 凭据并截断长度。
func notifySanitizeError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if start := strings.Index(text, "://"); start >= 0 {
		rest := text[start+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			if slash := strings.IndexAny(rest, "/ \t\n"); slash < 0 || at < slash {
				text = text[:start+3] + "***@" + rest[at+1:]
			}
		}
	}
	if len(text) > 500 {
		text = text[:500] + "…（已截断）"
	}
	return text
}

// notifySensitiveKeyParts 是「值必须掩码」的键名片段（小写比较）。
var notifySensitiveKeyParts = []string{
	"password", "passwd", "token", "secret", "api_key", "apikey",
	"authorization", "credential", "cookie", "private_key", "access_key",
}

// notifyIsSensitiveKey 判断配置键的值是否需要掩码。
func notifyIsSensitiveKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "" {
		return false
	}
	for _, part := range notifySensitiveKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

// notifyMaskedConfig 深拷贝配置并掩码敏感值与 URL userinfo（存储保持原值）。
func notifyMaskedConfig(config map[string]any) map[string]any {
	if config == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(config))
	for key, value := range config {
		if notifyIsSensitiveKey(key) {
			if text, ok := value.(string); !ok || strings.TrimSpace(text) != "" {
				out[key] = "***"
			} else {
				out[key] = value
			}
			continue
		}
		if strings.EqualFold(key, "url") {
			if text, ok := value.(string); ok {
				out[key] = notifyRedactURL(text)
				continue
			}
		}
		out[key] = notifyMaskValue(value)
	}
	return out
}

// notifyMaskValue 递归处理嵌套对象/数组。
func notifyMaskValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return notifyMaskedConfig(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = notifyMaskValue(item)
		}
		return out
	default:
		return value
	}
}

// notifyMaskedChannel 返回掩码后的渠道副本（不修改传入值，存储仍保存原文）。
func notifyMaskedChannel(channel pg.NotificationChannel) pg.NotificationChannel {
	masked := channel
	masked.Config = notifyMaskedConfig(channel.Config)
	return masked
}

// ---------- 敏感配置加密（B8） ----------

// 存储态加密标记：{"__enc":true,"payload":"enc:v1:...","url":"<去 userinfo 的 url>"}。
const (
	notifyEncryptedMarker  = "__enc"
	notifyEncryptedPayload = "payload"
)

// errNotificationSecretUnavailable 表示敏感配置需要证据存储加密但未装配。
var errNotificationSecretUnavailable = errors.New("通知敏感配置需要证据存储提供加密密钥")

// notifyConfigHasSensitive 递归判断配置是否含敏感值（键名命中或 URL userinfo）。
func notifyConfigHasSensitive(config map[string]any) bool {
	for key, value := range config {
		if notifyIsSensitiveKey(key) {
			if text, ok := value.(string); !ok || strings.TrimSpace(text) != "" {
				return true
			}
		}
		if strings.EqualFold(key, "url") {
			if text, ok := value.(string); ok {
				if parsed, err := url.Parse(text); err == nil && parsed.User != nil {
					return true
				}
			}
		}
		switch nested := value.(type) {
		case map[string]any:
			if notifyConfigHasSensitive(nested) {
				return true
			}
		case []any:
			for _, item := range nested {
				if object, ok := item.(map[string]any); ok && notifyConfigHasSensitive(object) {
					return true
				}
			}
		}
	}
	return false
}

// notifyURLWithoutUserinfo 返回去掉 userinfo 的 URL（用于存储态明文 url 字段）。
func notifyURLWithoutUserinfo(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}

// storedNotificationConfig 把请求中的明文配置转成存储态：含敏感值时整体加密，
// 只保留去掉 userinfo 的 url 供存储层做 webhook 必填校验。
func (o Options) storedNotificationConfig(config map[string]any) (map[string]any, error) {
	if config == nil {
		return map[string]any{}, nil
	}
	if !notifyConfigHasSensitive(config) {
		return config, nil
	}
	if o.Evidence == nil {
		return nil, errNotificationSecretUnavailable
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("序列化通知配置失败: %w", err)
	}
	token, err := o.Evidence.EncryptSecret(string(raw))
	if err != nil {
		return nil, fmt.Errorf("加密通知配置失败: %w", err)
	}
	stored := map[string]any{
		notifyEncryptedMarker:  true,
		notifyEncryptedPayload: token,
	}
	if urlText, ok := config["url"].(string); ok {
		stored["url"] = notifyURLWithoutUserinfo(urlText)
	}
	return stored, nil
}

// notificationPlaintextConfig 把存储态配置还原为明文；旧明文行原样返回。
func notificationPlaintextConfig(ev *evidence.Store, stored map[string]any) (config map[string]any, encrypted bool, err error) {
	if stored == nil {
		return map[string]any{}, false, nil
	}
	marker, _ := stored[notifyEncryptedMarker].(bool)
	if !marker {
		return stored, false, nil
	}
	encrypted = true
	if ev == nil {
		return nil, true, errNotificationSecretUnavailable
	}
	token, _ := stored[notifyEncryptedPayload].(string)
	plain, err := ev.DecryptSecret(token)
	if err != nil {
		return nil, true, err
	}
	if err := json.Unmarshal([]byte(plain), &config); err != nil {
		return nil, true, fmt.Errorf("通知配置密文内容不是合法 JSON: %w", err)
	}
	return config, true, nil
}

// notificationChannelPlaintext 解密渠道配置；旧明文敏感行做一次 best-effort 迁移。
func (o Options) notificationChannelPlaintext(ctx context.Context, channel pg.NotificationChannel) (pg.NotificationChannel, error) {
	config, encrypted, err := notificationPlaintextConfig(o.Evidence, channel.Config)
	if err != nil {
		return channel, err
	}
	channel.Config = config
	if !encrypted && o.Evidence != nil && o.Store != nil && notifyConfigHasSensitive(config) {
		if stored, storeErr := o.storedNotificationConfig(config); storeErr == nil {
			_ = o.Store.UpdateNotificationChannelStoredConfig(ctx, o.TenantID, channel.ID, stored)
		}
	}
	return channel, nil
}

// writeNotificationConfigError 把配置解密/加密错误映射成 HTTP 错误。
func writeNotificationConfigError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNotificationSecretUnavailable) {
		writeError(w, r, http.StatusServiceUnavailable, "evidence_unavailable",
			"通知敏感配置需要证据存储加密；请先配置证据密钥（BEACONX_EVIDENCE_KEY 或 evidence.key）")
		return
	}
	writeError(w, r, http.StatusInternalServerError, "internal_error", "通知配置加密/解密失败，请检查证据密钥是否匹配")
}

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
	masked := make([]pg.NotificationChannel, 0, len(channels))
	for _, channel := range channels {
		plain, err := o.notificationChannelPlaintext(r.Context(), channel)
		if err != nil {
			writeNotificationConfigError(w, r, err)
			return
		}
		masked = append(masked, notifyMaskedChannel(plain))
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": masked})
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
	storedConfig, err := o.storedNotificationConfig(req.Config)
	if err != nil {
		writeNotificationConfigError(w, r, err)
		return
	}
	channel, err := o.Store.CreateNotificationChannel(r.Context(), o.TenantID, pg.NotificationChannelInput{
		Name:    req.Name,
		Type:    req.Type,
		Config:  storedConfig,
		Enabled: enabled,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	plain, err := o.notificationChannelPlaintext(r.Context(), channel)
	if err != nil {
		writeNotificationConfigError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, notifyMaskedChannel(plain))
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
	plain, err := o.notificationChannelPlaintext(r.Context(), channel)
	if err != nil {
		writeNotificationConfigError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, notifyMaskedChannel(plain))
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
			storedConfig, err := o.storedNotificationConfig(config)
			if err != nil {
				writeNotificationConfigError(w, r, err)
				return
			}
			update.Config = storedConfig
		}
	}
	channel, err := o.Store.UpdateNotificationChannel(r.Context(), o.TenantID, channelID, update)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	plain, err := o.notificationChannelPlaintext(r.Context(), channel)
	if err != nil {
		writeNotificationConfigError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, notifyMaskedChannel(plain))
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
	channel, err = o.notificationChannelPlaintext(r.Context(), channel)
	if err != nil {
		writeNotificationConfigError(w, r, err)
		return
	}
	// 发送前先做 SSRF 校验：私网/回环地址直接 400，不发起请求也不写投递记录，
	// 让调用方明确知道「被平台拒绝」而不是目标不可达。
	if channel.Type == "webhook" {
		urlText, _ := channel.Config["url"].(string)
		if err := notifyValidateWebhookURL(r.Context(), urlText, notifyAllowPrivate()); err != nil {
			message := "webhook 目标地址被拒绝：" + notifySanitizeError(err)
			if !notifyAllowPrivate() && errors.Is(err, notifyErrPrivateAddress) {
				message += "（如需允许内网目标，请开启 " + notifyAllowPrivateEnv + "）"
			}
			writeError(w, r, http.StatusBadRequest, "webhook_blocked", message)
			return
		}
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

	client := notifyHTTPClient(notifyAllowPrivate(), notificationTestTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return "failed", "发送测试请求失败: " + notifySanitizeError(err)
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

// ---------- 事件推送 dispatcher（B8） ----------

// notifyPushEventTypes 是需要推送的任务事件类型。
func notifyPushEventTypes() map[string]bool {
	return map[string]bool{
		"approval_required": true,
		"completed":         true,
		"failed":            true,
		"paused":            true,
	}
}

// NotificationDispatcher 后台轮询 mission_events 并按启用渠道推送通知。
//
// 生命周期由 main 控制（与 engine 同启停）。语义：
//   - 启动时从 notification_events 最近已处理 event_id 继续；若台账为空，从当前
//     mission_events 最大 id 开始，避免把历史事件重新推一遍；
//   - 每个事件只推给该事件租户下 enabled 的渠道；先写 notification_events 占位
//     （DB 唯一约束）再发送，实现同一 event+channel 至多一次；
//   - email/generic 不联网，记 skipped；webhook 失败记 failed；
//   - 投递详情只保留目标响应片段（≤512 字节），网络错误不携带 URL。
type NotificationDispatcher struct {
	store    *pg.Store
	evidence *evidence.Store
	log      *slog.Logger
	interval time.Duration

	cursorMu sync.Mutex
	cursor   int64

	startedMu sync.Mutex
	started   bool
	stopOnce  sync.Once
	stopCh    chan struct{}
	done      chan struct{}
}

// NewNotificationDispatcher 构造后台推送器；store 为 nil 时 Start 直接返回。
func NewNotificationDispatcher(store *pg.Store, ev *evidence.Store, log *slog.Logger) *NotificationDispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &NotificationDispatcher{
		store:    store,
		evidence: ev,
		log:      log.With("component", "notification-dispatcher"),
		interval: time.Second,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start 初始化游标并启动后台循环。
func (d *NotificationDispatcher) Start(ctx context.Context) {
	d.startedMu.Lock()
	if d.started {
		d.startedMu.Unlock()
		return
	}
	d.started = true
	d.startedMu.Unlock()

	if d.store == nil {
		close(d.done)
		return
	}
	cursor, err := d.store.LatestNotificationEventCursor(ctx)
	if err != nil {
		d.log.Warn("读取通知事件游标失败，从当前事件之后开始", "error", err)
	}
	if cursor == 0 {
		if maxID, err := d.store.MaxMissionEventID(ctx); err == nil {
			cursor = maxID
		}
	}
	d.cursorMu.Lock()
	d.cursor = cursor
	d.cursorMu.Unlock()
	go d.loop(ctx)
}

// Stop 停止后台循环并等待退出（可重复调用）。
func (d *NotificationDispatcher) Stop() {
	d.startedMu.Lock()
	started := d.started
	d.startedMu.Unlock()
	if !started {
		return
	}
	d.stopOnce.Do(func() { close(d.stopCh) })
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		d.log.Warn("通知推送器停止超时")
	}
}

func (d *NotificationDispatcher) loop(ctx context.Context) {
	defer close(d.done)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.processOnce(ctx)
		}
	}
}

// processOnce 处理一批新事件；游标按事件 id 前进（不因单个渠道失败而回退，
// 否则会无限重放）。
func (d *NotificationDispatcher) processOnce(ctx context.Context) {
	d.cursorMu.Lock()
	after := d.cursor
	d.cursorMu.Unlock()

	events, err := d.store.ListMissionEventsAfter(ctx, after, 100)
	if err != nil {
		d.log.Warn("读取事件推送队列失败", "error", err)
		return
	}
	for _, event := range events {
		if notifyPushEventTypes()[event.Type] {
			d.dispatchEvent(ctx, event)
		}
		d.cursorMu.Lock()
		if event.ID > d.cursor {
			d.cursor = event.ID
		}
		d.cursorMu.Unlock()
	}
}

// dispatchEvent 把单个事件投递给该租户的所有启用渠道。
func (d *NotificationDispatcher) dispatchEvent(ctx context.Context, event pg.MissionEvent) {
	channels, err := d.store.ListEnabledNotificationChannelsForPush(ctx, event.TenantID)
	if err != nil {
		d.log.Warn("读取推送渠道失败", "event_id", event.ID, "error", err)
		return
	}
	for _, channel := range channels {
		notificationID, fresh, err := d.store.BeginNotificationEvent(ctx, event.TenantID, event.ID, channel.ID)
		if err != nil {
			d.log.Warn("写入通知事件台账失败", "event_id", event.ID, "channel_id", channel.ID, "error", err)
			continue
		}
		if !fresh {
			continue
		}
		status, detail := d.deliver(ctx, event, channel)
		delivery, err := d.store.CreateNotificationDelivery(ctx, event.TenantID, channel.ID,
			event.Type, status, detail)
		if err != nil {
			d.log.Warn("写入推送投递记录失败", "event_id", event.ID, "channel_id", channel.ID, "error", err)
			continue
		}
		if err := d.store.CompleteNotificationEvent(ctx, event.TenantID, notificationID, delivery.ID); err != nil {
			d.log.Warn("回填通知事件投递标识失败", "event_id", event.ID, "error", err)
		}
	}
}

// deliver 执行一次渠道投递，返回投递状态与脱敏后的详情。
func (d *NotificationDispatcher) deliver(ctx context.Context, event pg.MissionEvent, channel pg.NotificationChannel) (status, detail string) {
	if channel.Type != "webhook" {
		return "skipped", fmt.Sprintf("%s 渠道不实际发送，事件推送记为 skipped", channel.Type)
	}
	config, _, err := notificationPlaintextConfig(d.evidence, channel.Config)
	if err != nil {
		return "failed", "渠道配置解密失败"
	}
	urlText, _ := config["url"].(string)
	if err := notifyValidateWebhookURL(ctx, urlText, notifyAllowPrivate()); err != nil {
		return "failed", "webhook 目标被拒绝：" + notifySanitizeError(err)
	}
	payload, err := json.Marshal(map[string]any{
		"event": map[string]any{
			"id":         event.ID,
			"mission_id": event.MissionID,
			"type":       event.Type,
			"actor":      event.Actor,
			"reason":     event.Reason,
			"payload":    event.Payload,
			"created_at": event.CreatedAt,
		},
		"channel": map[string]any{"id": channel.ID, "name": channel.Name, "type": channel.Type},
	})
	if err != nil {
		return "failed", "构造推送消息失败"
	}
	sendCtx, cancel := context.WithTimeout(ctx, notificationTestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, urlText, bytes.NewReader(payload))
	if err != nil {
		return "failed", "构造推送请求失败"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "BeaconX-Notification/1.0")

	resp, err := notifyHTTPClient(notifyAllowPrivate(), notificationTestTimeout).Do(req)
	if err != nil {
		// 不把底层错误（可能含目标 URL）写给投递台账。
		return "failed", "webhook 请求失败（网络错误或目标不可达）"
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, notificationTestBodyLimit))
	detail = notifyPushDetail(resp.StatusCode, body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "sent", detail
	}
	return "failed", detail
}

// notifyPushDetail 生成脱敏且不超过 512 字节的投递详情。
func notifyPushDetail(statusCode int, body []byte) string {
	detail := fmt.Sprintf("HTTP %d", statusCode)
	if text := strings.TrimSpace(string(body)); text != "" {
		detail += "：" + notifySanitizeError(errors.New(text))
	}
	return notifyTruncateBytes(detail, notificationTestBodyLimit)
}

// notifyTruncateBytes 按 UTF-8 安全截断字符串并追加省略号。
func notifyTruncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
