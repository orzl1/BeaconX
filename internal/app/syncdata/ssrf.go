// Package syncdata 是外部数据源同步（B5 Sync）的执行器与适配器。
//
// 职责边界：
//   - 本包只做「拉取 + 解析 + 写入」，不持有 HTTP 路由，也不依赖 httpapi；
//   - 数据源配置、项目与运行记录的持久化在 internal/infra/pg（store_sync.go）；
//   - 运行是异步的：HTTP 层创建 sync_runs 记录后调用 Start，由本包在后台完成
//     并把结果写回运行记录（running → succeeded / failed），前端轮询运行记录。
//
// 安全边界（SSRF）：
//   - 同步 URL 只允许 http/https；
//   - 默认拒绝回环、私网、链路本地、CGNAT 等内网地址，`BEACONX_SYNC_ALLOW_PRIVATE=true`
//     时才允许（对应配置项 sync.allow_private，默认 false）；
//   - 校验发生在「拨号前对目标 IP」这一步（而不是只校验 URL 主机名），因此
//     DNS 重绑定无法绕过；
//   - 不跟随 HTTP 重定向（重定向是绕过域名校验的常见手法，需要时由管理员显式配置）；
//   - 错误信息会对 URL 凭据做脱敏，不会把 userinfo 写进运行记录。
package syncdata

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 默认限制：10 秒超时、5MB 响应体（与任务契约一致）。
const (
	DefaultTimeout      = 10 * time.Second
	DefaultMaxBodyBytes = 5 << 20
	DefaultRunTimeout   = 60 * time.Second
)

// ErrPrivateAddress 表示目标解析到内网/回环地址且未开启 allow_private。
var ErrPrivateAddress = errors.New("目标地址属于回环或内网，已拒绝访问（如需允许请开启 sync.allow_private）")

// ErrRedirectNotAllowed 表示上游返回了重定向；同步只接受直接响应。
var ErrRedirectNotAllowed = errors.New("同步请求不跟随 HTTP 重定向")

// Config 是同步执行器的运行配置。
type Config struct {
	// AllowPrivate 允许访问回环/私网地址；默认 false。
	AllowPrivate bool
	// Timeout 是单次 HTTP 请求超时；<=0 时取 DefaultTimeout。
	Timeout time.Duration
	// MaxBodyBytes 是响应体上限；<=0 时取 DefaultMaxBodyBytes。
	MaxBodyBytes int64
	// RunTimeout 是一次同步任务的总超时；<=0 时取 DefaultRunTimeout。
	RunTimeout time.Duration
}

// ConfigFromEnv 从环境变量读取同步配置。
//
// 只支持环境变量（BEACONX_SYNC_ALLOW_PRIVATE / BEACONX_SYNC_TIMEOUT /
// BEACONX_SYNC_MAX_BYTES）：config.Config 结构体不属于本批改动范围，
// 后续把它并入配置文件时只需改这里。
func ConfigFromEnv() Config {
	cfg := Config{}
	if raw := strings.TrimSpace(os.Getenv("BEACONX_SYNC_ALLOW_PRIVATE")); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			cfg.AllowPrivate = enabled
		}
	}
	if raw := strings.TrimSpace(os.Getenv("BEACONX_SYNC_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			cfg.Timeout = d
		}
	}
	if raw := strings.TrimSpace(os.Getenv("BEACONX_SYNC_MAX_BYTES")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			cfg.MaxBodyBytes = n
		}
	}
	return cfg.normalized()
}

// normalized 补齐默认值。
func (c Config) normalized() Config {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = DefaultRunTimeout
	}
	return c
}

// 需要额外拦截的保留/特殊地址段（net.IP 的 IsPrivate 不覆盖这些）。
var blockedCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"100.64.0.0/10", // CGNAT（运营商级 NAT，通常指向内网）
		"192.0.0.0/24",  // IETF 协议分配
		"198.18.0.0/15", // 基准测试保留段
		"240.0.0.0/4",   // 保留（含 255.255.255.255）
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		if _, network, err := net.ParseCIDR(raw); err == nil {
			out = append(out, network)
		}
	}
	return out
}()

// ipDisallowed 报告某个 IP 是否禁止访问（allowPrivate=false 时使用）。
func ipDisallowed(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, network := range blockedCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// validateTarget 校验一次拉取目标：scheme、host 与解析出的全部 IP。
//
// 返回解析后的 IP 列表，供拨号时直接使用（避免二次解析被 DNS 重绑定利用）。
func validateTarget(ctx context.Context, rawURL string, allowPrivate bool) ([]net.IP, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("同步 URL 不合法")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("同步 URL 只支持 http/https")
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("同步 URL 缺少主机")
	}
	// IP 字面量直接校验，不发起 DNS 查询。
	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && ipDisallowed(ip) {
			return nil, ErrPrivateAddress
		}
		return []net.IP{ip}, nil
	}
	resolver := net.DefaultResolver
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("解析同步目标主机失败")
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("同步目标主机没有可用地址")
	}
	out := make([]net.IP, 0, len(ips))
	for _, item := range ips {
		if !allowPrivate && ipDisallowed(item.IP) {
			return nil, ErrPrivateAddress
		}
		out = append(out, item.IP)
	}
	return out, nil
}

// newHTTPClient 构造带 SSRF 防护的同步 HTTP 客户端。
//
// 关键点：拨号时校验「真正要连的 IP」，而不是只校验 URL 里的主机名——后者在
// DNS 重绑定场景下几分钟后就能绕过。另外禁用重定向与代理（环境变量里的
// HTTP_PROXY 可能把请求带进内网）。
func newHTTPClient(allowPrivate bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("解析同步目标地址失败")
			}
			ips, err := validateTarget(ctx, "http://"+net.JoinHostPort(host, port), allowPrivate)
			if err != nil {
				return nil, err
			}
			// 只用第一个已校验的 IP 拨号，杜绝「校验一个、连另一个」。
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectNotAllowed
		},
	}
}

// redactMessage 对错误文本里的 URL 凭据做脱敏。
//
// 两层处理：先按「scheme://user:pass@」形态整体替换，再把仍可能残留的
// userinfo 片段替换成 ***。运行记录会长期保存错误文本，不能把凭据留在库里。
func redactMessage(message string) string {
	if message == "" {
		return ""
	}
	if idx := strings.Index(message, "://"); idx >= 0 {
		rest := message[idx+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			if slash := strings.IndexAny(rest, "/ \t\n"); slash < 0 || at < slash {
				message = message[:idx+3] + "***@" + rest[at+1:]
			}
		}
	}
	if len(message) > 1000 {
		message = message[:1000] + "…（已截断）"
	}
	return message
}
