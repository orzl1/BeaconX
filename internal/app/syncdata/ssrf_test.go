package syncdata

import (
	"context"
	"net"
	"strings"
	"testing"
)

// SSRF 防护与脱敏的纯单元测试（不依赖数据库）。

func TestIPDisallowed(t *testing.T) {
	blocked := []string{
		"127.0.0.1",       // loopback
		"10.1.2.3",        // 私网
		"172.16.0.1",      // 私网
		"192.168.1.1",     // 私网
		"169.254.169.254", // 链路本地（云元数据）
		"100.64.0.1",      // CGNAT
		"198.18.0.1",      // 基准测试保留段
		"240.0.0.1",       // 保留段
		"0.0.0.0",         // 未指定
		"224.0.0.1",       // 组播
		"::1",             // IPv6 loopback
		"fc00::1",         // IPv6 唯一本地
		"fe80::1",         // IPv6 链路本地
	}
	for _, raw := range blocked {
		if ip := net.ParseIP(raw); ip == nil {
			t.Fatalf("测试用例 IP 非法: %s", raw)
		} else if !ipDisallowed(ip) {
			t.Errorf("%s 应被判定为禁止访问", raw)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "203.0.113.10", "2001:4860:4860::8888"}
	for _, raw := range allowed {
		if ip := net.ParseIP(raw); ip == nil {
			t.Fatalf("测试用例 IP 非法: %s", raw)
		} else if ipDisallowed(ip) {
			t.Errorf("%s 不应被判定为禁止访问", raw)
		}
	}
	if !ipDisallowed(nil) {
		t.Error("nil IP 应视为禁止访问（fail closed）")
	}
}

func TestValidateTarget(t *testing.T) {
	ctx := context.Background()

	t.Run("只允许 http/https", func(t *testing.T) {
		for _, raw := range []string{"ftp://example.com/x", "file:///etc/passwd", "gopher://example.com"} {
			if _, err := validateTarget(ctx, raw, false); err == nil {
				t.Errorf("%s 应被拒绝", raw)
			}
		}
		if _, err := validateTarget(ctx, "", false); err == nil {
			t.Error("空 URL 应被拒绝")
		}
		if _, err := validateTarget(ctx, "http:///path", false); err == nil {
			t.Error("缺少主机应被拒绝")
		}
	})

	t.Run("回环默认拒绝，allow_private 放行", func(t *testing.T) {
		for _, raw := range []string{
			"http://127.0.0.1:8080/x",
			"http://[::1]:8080/x",
			"http://localhost:8080/x",
			"http://10.0.0.5/x",
		} {
			if _, err := validateTarget(ctx, raw, false); err == nil {
				t.Errorf("%s 默认应被拒绝", raw)
			}
			if _, err := validateTarget(ctx, raw, true); err != nil {
				t.Errorf("%s 在 allow_private 下应被放行，实际 %v", raw, err)
			}
		}
	})

	t.Run("公网地址放行", func(t *testing.T) {
		ips, err := validateTarget(ctx, "https://8.8.8.8/resolve", false)
		if err != nil {
			t.Fatalf("公网 IP 应被放行: %v", err)
		}
		if len(ips) != 1 || !ips[0].Equal(net.ParseIP("8.8.8.8")) {
			t.Fatalf("解析结果异常: %v", ips)
		}
	})
}

func TestRedactMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 空表示只断言不包含敏感片段
	}{
		{name: "空字符串", in: "", want: ""},
		{name: "普通错误原样", in: "上游返回 HTTP 500", want: "上游返回 HTTP 500"},
		{
			name: "URL 里的 userinfo 被脱敏",
			in:   `Get "http://user:secret@example.com/x": connection refused`,
		},
		{
			name: "无 userinfo 的 URL 不受影响",
			in:   `Get "http://example.com/x": connection refused`,
			want: `Get "http://example.com/x": connection refused`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactMessage(tc.in)
			if tc.want != "" && got != tc.want {
				t.Fatalf("redactMessage(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "secret") {
				t.Fatalf("脱敏后仍包含密码: %q", got)
			}
			if strings.Contains(got, "user:") {
				t.Fatalf("脱敏后仍包含 userinfo: %q", got)
			}
		})
	}

	long := strings.Repeat("错", 2000)
	if got := redactMessage(long); len(got) > 1100 {
		t.Fatalf("超长错误未截断：%d 字符", len(got))
	}
}

func TestConfigNormalized(t *testing.T) {
	cfg := Config{}.normalized()
	if cfg.Timeout != DefaultTimeout || cfg.MaxBodyBytes != DefaultMaxBodyBytes || cfg.RunTimeout != DefaultRunTimeout {
		t.Fatalf("默认值未补齐: %+v", cfg)
	}
	if got := (Config{Timeout: 3, MaxBodyBytes: 10, RunTimeout: 5}).normalized(); got.Timeout != 3 {
		t.Fatalf("显式值被覆盖: %+v", got)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("BEACONX_SYNC_ALLOW_PRIVATE", "true")
	t.Setenv("BEACONX_SYNC_TIMEOUT", "3s")
	t.Setenv("BEACONX_SYNC_MAX_BYTES", "2048")
	cfg := ConfigFromEnv()
	if !cfg.AllowPrivate {
		t.Fatal("BEACONX_SYNC_ALLOW_PRIVATE=true 未生效")
	}
	if cfg.Timeout.Seconds() != 3 || cfg.MaxBodyBytes != 2048 {
		t.Fatalf("环境变量未生效: %+v", cfg)
	}
}
