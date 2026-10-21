package asset

import (
	"strings"
	"testing"
)

// TestKindValid 覆盖全部合法类型与若干非法取值（含大小写错误、空串与旧名）。
func TestKindValid(t *testing.T) {
	valid := []Kind{
		KindRootDomain, KindSubdomain, KindIP, KindService, KindApp, KindEndpoint,
	}
	for _, k := range valid {
		if !k.Valid() {
			t.Errorf("类型 %q 应合法", string(k))
		}
		if k.String() == string(k) {
			t.Errorf("类型 %q 缺少中文字称", string(k))
		}
	}
	invalid := []Kind{"", "domain", "RootDomain", "service ", "cidr", "unknown"}
	for _, k := range invalid {
		if k.Valid() {
			t.Errorf("类型 %q 应非法", string(k))
		}
	}
}

// TestKindWireValues 锁定序列化取值：这些字符串直接写入数据库，
// 改动会造成存量数据不兼容。
func TestKindWireValues(t *testing.T) {
	cases := map[Kind]string{
		KindRootDomain: "root_domain",
		KindSubdomain:  "subdomain",
		KindIP:         "ip",
		KindService:    "service",
		KindApp:        "app",
		KindEndpoint:   "endpoint",
	}
	for kind, want := range cases {
		if string(kind) != want {
			t.Errorf("类型取值 = %q，期望 %q", string(kind), want)
		}
	}
}

// TestNormalizeDomain 覆盖大小写、结尾点、单标签、长度边界与各类非法输入。
func TestNormalizeDomain(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string // 非空表示期望报错，且错误信息需包含该片段
	}{
		{name: "已归一的根域", raw: "example.com", want: "example.com"},
		{name: "大写与小写混合", raw: "Example.COM", want: "example.com"},
		{name: "去首尾空白", raw: "  example.com\t", want: "example.com"},
		{name: "结尾点（绝对域名）", raw: "example.com.", want: "example.com"},
		{name: "多个结尾点", raw: "Example.COM..", want: "example.com"},
		{name: "多级子域", raw: "API.V2.Example.COM", want: "api.v2.example.com"},
		{name: "单标签内网主机", raw: "intranet", want: "intranet"},
		{name: "连字符在中间", raw: "a-b.c-d.example.com", want: "a-b.c-d.example.com"},
		{name: "punycode", raw: "xn--fiqs8s.example", want: "xn--fiqs8s.example"},
		{name: "数字标签（非 TLD）", raw: "10.example.com", want: "10.example.com"},
		{name: "单标签 63 字节", raw: strings.Repeat("a", 63) + ".com", want: strings.Repeat("a", 63) + ".com"},

		{name: "空串", raw: "", wantErr: "值为空"},
		{name: "纯空白", raw: "   ", wantErr: "值为空"},
		{name: "只有点", raw: ".", wantErr: "结尾的点"},
		{name: "只有多个点", raw: "...", wantErr: "结尾的点"},
		{name: "IP 字面量", raw: "1.2.3.4", wantErr: "请使用 ip 类型"},
		{name: "IPv6 字面量", raw: "::1", wantErr: "请使用 ip 类型"},
		{name: "含协议", raw: "https://example.com", wantErr: "请使用 endpoint 类型"},
		{name: "含端口", raw: "example.com:8080", wantErr: "请使用 service 类型"},
		{name: "含路径", raw: "example.com/admin", wantErr: "不得包含路径"},
		{name: "含查询", raw: "example.com?a=1", wantErr: "不得包含路径"},
		{name: "含下划线", raw: "exa_mple.com", wantErr: "非法字符"},
		{name: "含空格", raw: "exa mple.com", wantErr: "空格"},
		{name: "通配符", raw: "*.example.com", wantErr: "非法字符"},
		{name: "连字符开头", raw: "-example.com", wantErr: "连字符开头或结尾"},
		{name: "连字符结尾", raw: "example-.com", wantErr: "连字符开头或结尾"},
		{name: "空标签", raw: "a..example.com", wantErr: "空标签"},
		{name: "标签超长（64）", raw: strings.Repeat("a", 64) + ".com", wantErr: "标签过长"},
		{name: "域名超长（254）", raw: strings.Repeat("a", 250) + ".com", wantErr: "域名过长"},
		{name: "顶级标签纯数字", raw: "1.2.3", wantErr: "顶级标签不得为纯数字"},
		{name: "单标签纯数字", raw: "12345", wantErr: "顶级标签不得为纯数字"},
		{name: "含中文", raw: "例子.example", wantErr: "非法字符"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDomain(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeDomain(%q) 期望报错，实际得到 %q", tc.raw, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
				}
				if got != "" {
					t.Fatalf("报错时应返回空键，实际 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeDomain(%q) 意外报错：%v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeDomain(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeIP 覆盖 IPv4/IPv6 规范化、映射地址、CIDR 与端口等非法形态。
func TestNormalizeIP(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "IPv4", raw: "1.2.3.4", want: "1.2.3.4"},
		{name: "IPv4 去空白", raw: "  10.0.0.1 ", want: "10.0.0.1"},
		{name: "IPv6 压缩", raw: "2001:0db8:0000:0000:0000:0000:0000:0001", want: "2001:db8::1"},
		{name: "IPv6 回环", raw: "::1", want: "::1"},
		{name: "IPv6 大写", raw: "2001:DB8::1", want: "2001:db8::1"},
		{name: "IPv4 映射的 IPv6 归一为 IPv4", raw: "::ffff:1.2.3.4", want: "1.2.3.4"},

		{name: "空串", raw: "", wantErr: "值为空"},
		{name: "纯空白", raw: " ", wantErr: "值为空"},
		{name: "CIDR", raw: "10.0.0.0/24", wantErr: "请使用 NormalizeCIDR"},
		{name: "越界的 IPv4", raw: "1.2.3.256", wantErr: "无效的 IP"},
		{name: "前导零的 IPv4", raw: "010.1.1.1", wantErr: "无效的 IP"},
		{name: "域名", raw: "example.com", wantErr: "无效的 IP"},
		{name: "带端口", raw: "1.2.3.4:80", wantErr: "无效的 IP"},
		{name: "带方括号", raw: "[::1]", wantErr: "无效的 IP"},
		{name: "带 zone", raw: "fe80::1%eth0", wantErr: "无效的 IP"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeIP(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeIP(%q) 期望报错，实际得到 %q", tc.raw, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeIP(%q) 意外报错：%v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeIP(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeCIDR 覆盖主机位抹除、IPv6 压缩与非法前缀。
func TestNormalizeCIDR(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "标准 IPv4 网段", raw: "10.0.0.0/24", want: "10.0.0.0/24"},
		{name: "抹除主机位", raw: "10.0.0.5/24", want: "10.0.0.0/24"},
		{name: "去空白", raw: " 192.168.0.0/16 ", want: "192.168.0.0/16"},
		{name: "IPv6 压缩并抹主机位", raw: "2001:db8::1/32", want: "2001:db8::/32"},
		{name: "默认路由", raw: "0.0.0.0/0", want: "0.0.0.0/0"},
		{name: "IPv6 默认路由", raw: "::/0", want: "::/0"},
		{name: "单主机网段", raw: "192.0.2.7/32", want: "192.0.2.7/32"},

		{name: "空串", raw: "", wantErr: "值为空"},
		{name: "缺少前缀长度", raw: "10.0.0.0", wantErr: "必须包含前缀长度"},
		{name: "前缀越界", raw: "10.0.0.0/33", wantErr: "无效的 CIDR"},
		{name: "前缀非数字", raw: "10.0.0.0/ab", wantErr: "无效的 CIDR"},
		{name: "主机不是地址", raw: "example.com/24", wantErr: "无效的 CIDR"},
		{name: "空前缀", raw: "10.0.0.0/", wantErr: "无效的 CIDR"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeCIDR(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeCIDR(%q) 期望报错，实际得到 %q", tc.raw, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeCIDR(%q) 意外报错：%v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeCIDR(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeURL 覆盖协议、默认端口、fragment、空路径、query 原样、
// IPv6 主机与 userinfo 等规则。
func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "最简 URL", raw: "http://example.com", want: "http://example.com"},
		{name: "大写协议与主机", raw: "HTTP://EXAMPLE.COM/", want: "http://example.com"},
		{name: "空路径与斜杠等价", raw: "https://api.example.com/", want: "https://api.example.com"},
		{name: "https 默认端口被去掉", raw: "https://api.example.com:443/v1/users", want: "https://api.example.com/v1/users"},
		{name: "http 默认端口被去掉", raw: "http://example.com:80/a", want: "http://example.com/a"},
		{name: "非默认端口保留", raw: "https://example.com:8443/a", want: "https://example.com:8443/a"},
		{name: "端口前导零", raw: "http://example.com:0080/a", want: "http://example.com/a"},
		{name: "去掉 fragment", raw: "http://example.com/a#section", want: "http://example.com/a"},
		{name: "保留路径与 query 原序", raw: "https://example.com:443/v1/users?b=2&a=1", want: "https://example.com/v1/users?b=2&a=1"},
		{name: "根路径加 query", raw: "http://example.com/?q=1", want: "http://example.com?q=1"},
		{name: "非根路径的结尾斜杠保留", raw: "http://example.com/a/", want: "http://example.com/a/"},
		{name: "百分号编码归一", raw: "http://example.com/%7euser", want: "http://example.com/~user"},
		{name: "路径空格重新转义", raw: "http://example.com/a%20b?x=1", want: "http://example.com/a%20b?x=1"},
		{name: "IPv4 主机", raw: "http://1.2.3.4:8080/", want: "http://1.2.3.4:8080"},
		{name: "IPv6 主机保留方括号", raw: "https://[2001:db8::1]:8443/x", want: "https://[2001:db8::1]:8443/x"},
		{name: "IPv6 默认端口", raw: "http://[::1]:80/x", want: "http://[::1]/x"},
		{name: "空 query 不输出问号", raw: "http://example.com?", want: "http://example.com"},

		{name: "空串", raw: "", wantErr: "值为空"},
		{name: "缺少协议", raw: "example.com/a", wantErr: "必须显式包含 http/https"},
		{name: "协议相对 URL", raw: "//example.com/a", wantErr: "必须显式包含 http/https"},
		{name: "非 http 协议", raw: "ftp://example.com/a", wantErr: "只支持 http/https"},
		{name: "javascript 伪协议", raw: "javascript:alert(1)", wantErr: "只支持 http/https"},
		{name: "缺少主机", raw: "http:///a", wantErr: "缺少主机名"},
		{name: "主机含下划线", raw: "http://exa_mple.com/", wantErr: "主机非法"},
		{name: "端口越界", raw: "http://example.com:99999/", wantErr: "端口超出范围"},
		{name: "含 userinfo", raw: "http://user:pw@example.com/", wantErr: "用户信息"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeURL(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeURL(%q) 期望报错，实际得到 %q", tc.raw, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeURL(%q) 意外报错：%v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeURL(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeURLIdempotent 验证归一化是幂等的：对已归一化的键再跑一次
// 必须得到同样的结果，否则存储层的 upsert 会反复产生新键。
func TestNormalizeURLIdempotent(t *testing.T) {
	inputs := []string{
		"http://example.com",
		"https://api.example.com:8443/v1/users?b=2&a=1",
		"http://example.com/a/",
		"https://[2001:db8::1]:8443/x",
		"http://example.com/%7euser",
	}
	for _, in := range inputs {
		first, err := NormalizeURL(in)
		if err != nil {
			t.Fatalf("NormalizeURL(%q) 意外报错：%v", in, err)
		}
		second, err := NormalizeURL(first)
		if err != nil {
			t.Fatalf("对已归一化的 %q 再次归一化失败：%v", first, err)
		}
		if first != second {
			t.Errorf("归一化不幂等：%q → %q → %q", in, first, second)
		}
	}
}

// TestNormalizeService 覆盖主机类型、端口边界、IPv6 方括号与非法形态。
func TestNormalizeService(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "域名加端口", raw: "example.com:443", want: "example.com:443"},
		{name: "主机大写与端口前导零", raw: "Example.COM:0443", want: "example.com:443"},
		{name: "去首尾空白", raw: "  db.example.com:5432  ", want: "db.example.com:5432"},
		{name: "IP 加端口", raw: "10.0.0.1:3306", want: "10.0.0.1:3306"},
		{name: "IPv6 方括号保留", raw: "[2001:db8::1]:6379", want: "[2001:db8::1]:6379"},
		{name: "IPv6 大写压缩", raw: "[2001:DB8:0:0::1]:6379", want: "[2001:db8::1]:6379"},
		{name: "单标签主机", raw: "localhost:22", want: "localhost:22"},
		{name: "端口下界", raw: "example.com:1", want: "example.com:1"},
		{name: "端口上界", raw: "example.com:65535", want: "example.com:65535"},

		{name: "空串", raw: "", wantErr: "值为空"},
		{name: "缺少端口", raw: "example.com", wantErr: "主机:端口"},
		{name: "端口为空", raw: "example.com:", wantErr: "端口为空"},
		{name: "端口为零", raw: "example.com:0", wantErr: "超出范围"},
		{name: "端口越界", raw: "example.com:65536", wantErr: "超出范围"},
		{name: "端口为负", raw: "example.com:-1", wantErr: "十进制数字"},
		{name: "端口为服务名", raw: "example.com:http", wantErr: "十进制数字"},
		{name: "缺少主机", raw: ":80", wantErr: "缺少主机"},
		{name: "主机非法", raw: "exa_mple.com:80", wantErr: "服务主机非法"},
		{name: "端口后带路径", raw: "example.com:80/path", wantErr: "十进制数字"},
		{name: "未加方括号的 IPv6", raw: "::1:443", wantErr: "主机:端口"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeService(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeService(%q) 期望报错，实际得到 %q", tc.raw, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeService(%q) 意外报错：%v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeService(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalize 覆盖按类型分发、应用名大小写保留与错误包装。
func TestNormalize(t *testing.T) {
	cases := []struct {
		name    string
		kind    Kind
		raw     string
		want    Identity
		wantErr string
	}{
		{
			name: "子域",
			kind: KindSubdomain,
			raw:  "  API.Example.COM. ",
			want: Identity{Kind: KindSubdomain, Key: "api.example.com"},
		},
		{
			name: "根域",
			kind: KindRootDomain,
			raw:  "Example.com",
			want: Identity{Kind: KindRootDomain, Key: "example.com"},
		},
		{
			name: "IP",
			kind: KindIP,
			raw:  "2001:0db8::1",
			want: Identity{Kind: KindIP, Key: "2001:db8::1"},
		},
		{
			name: "服务",
			kind: KindService,
			raw:  "Example.com:80",
			want: Identity{Kind: KindService, Key: "example.com:80"},
		},
		{
			name: "应用名保留大小写",
			kind: KindApp,
			raw:  "  com.Example.App  ",
			want: Identity{Kind: KindApp, Key: "com.Example.App"},
		},
		{
			name: "端点",
			kind: KindEndpoint,
			raw:  "HTTP://Example.com/",
			want: Identity{Kind: KindEndpoint, Key: "http://example.com"},
		},
		{
			name:    "未知类型",
			kind:    Kind("domain"),
			raw:     "example.com",
			wantErr: "未知的资产类型",
		},
		{
			name:    "空类型",
			kind:    Kind(""),
			raw:     "example.com",
			wantErr: "未知的资产类型",
		},
		{
			name:    "应用名为空",
			kind:    KindApp,
			raw:     "   ",
			wantErr: "应用归一化失败",
		},
		{
			name:    "端点非法时错误带类型",
			kind:    KindEndpoint,
			raw:     "example.com",
			wantErr: "端点归一化失败",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.kind, tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Normalize(%q, %q) 期望报错，实际得到 %+v", tc.kind, tc.raw, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
				}
				if got != (Identity{}) {
					t.Fatalf("报错时应返回零值 Identity，实际 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q, %q) 意外报错：%v", tc.kind, tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q, %q) = %+v，期望 %+v", tc.kind, tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeSharesKeyAcrossKinds 验证“同一资产的等价写法”在各类型内
// 收敛到同一个键（大小写、结尾点、默认端口、fragment、主机位）。
func TestNormalizeSharesKeyAcrossKinds(t *testing.T) {
	groups := []struct {
		name string
		kind Kind
		raws []string
	}{
		{
			name: "域名大小写与结尾点",
			kind: KindRootDomain,
			raws: []string{"example.com", "Example.Com", "example.com.", "  EXAMPLE.COM.  "},
		},
		{
			name: "端点默认端口与 fragment",
			kind: KindEndpoint,
			raws: []string{
				"http://example.com/a",
				"http://example.com:80/a",
				"HTTP://Example.COM/a#frag",
			},
		},
		{
			name: "服务端口前导零",
			kind: KindService,
			raws: []string{"example.com:443", "Example.COM:0443"},
		},
		{
			name: "IPv6 不同写法",
			kind: KindIP,
			raws: []string{"2001:db8::1", "2001:0db8:0000:0000:0000:0000:0000:0001", "2001:DB8::1"},
		},
	}

	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			var first Identity
			for i, raw := range g.raws {
				id, err := Normalize(g.kind, raw)
				if err != nil {
					t.Fatalf("Normalize(%q, %q) 意外报错：%v", g.kind, raw, err)
				}
				if i == 0 {
					first = id
					continue
				}
				if id != first {
					t.Errorf("%q 的键 = %q，期望与 %q 的键 %q 相同", raw, id.Key, g.raws[0], first.Key)
				}
			}
		})
	}
}
