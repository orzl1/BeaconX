// Package asset 定义资产的**归一化身份**：把外部输入（域名 / IP / CIDR /
// URL / 主机:端口 / 应用名）折叠成稳定、可比较、可用于唯一索引的字符串键。
//
// 设计的出发点：资产图是跨任务共享的真值库，同一个被测对象无论被多少个任务、
// 多少次、以何种书写形式发现，都必须落到**同一行**。因此本包只做两件事：
//
//  1. 把书写形式收敛为规范形式（大小写、默认端口、结尾点、fragment、CIDR 主机位）；
//  2. 拒绝不能作为身份的值（空值、非法域名、含路径的域名、裸 IP 冒充 CIDR 等），
//     让非法输入在写入存储之前就失败，而不是在库里留下两行“看起来一样”的资产。
//
// 本包是纯领域逻辑：不依赖数据库、不依赖其他内部包，只使用标准库。
package asset

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Kind 是资产类型。取值与存储层（schema 里的 assets.type）保持一致，
// 序列化后即为数据库中的字面量，不要随意改动。
type Kind string

const (
	// KindRootDomain 是根域（注册域，eTLD+1），例如 example.com。
	KindRootDomain Kind = "root_domain"
	// KindSubdomain 是子域（完整主机名），例如 api.example.com。
	KindSubdomain Kind = "subdomain"
	// KindIP 是单个 IP 地址（IPv4 或 IPv6），不含前缀长度。
	KindIP Kind = "ip"
	// KindService 是“主机:端口”形式的网络服务，例如 db.example.com:5432。
	KindService Kind = "service"
	// KindApp 是移动/桌面应用，以应用名或 bundle id 为身份。
	KindApp Kind = "app"
	// KindEndpoint 是 HTTP 接口，以归一化后的 URL 为身份（HTTP 方法由存储层
	// 单独组成复合键，不在本包内）。
	KindEndpoint Kind = "endpoint"
)

// Valid 报告 k 是否是已知的资产类型。
func (k Kind) Valid() bool {
	switch k {
	case KindRootDomain, KindSubdomain, KindIP, KindService, KindApp, KindEndpoint:
		return true
	}
	return false
}

// String 返回类型的中文字称，用于错误信息。
func (k Kind) String() string {
	switch k {
	case KindRootDomain:
		return "根域"
	case KindSubdomain:
		return "子域"
	case KindIP:
		return "IP"
	case KindService:
		return "服务"
	case KindApp:
		return "应用"
	case KindEndpoint:
		return "端点"
	}
	return string(k)
}

// Identity 是资产的归一化身份。Kind 加 Key 唯一确定一个资产；
// 存储层把二者作为唯一索引的一对列，Key 的格式由 Kind 决定。
type Identity struct {
	Kind Kind   `json:"kind"`
	Key  string `json:"key"`
}

// 各种输入的长度上限。域名的 253 与单标签 63 来自 DNS 规范；URL 与
// 服务地址没有规范上限，这里取一个足够宽松但仍然能挡住异常输入的界限。
const (
	maxDomainLen  = 253
	maxLabelLen   = 63
	maxHostnameLn = 253
	maxURLLen     = 2048
	maxServiceLen = maxHostnameLn + 1 + 5 // 主机 + 冒号 + 最大五位数端口
	maxAppLen     = 255
)

// errEmpty 是空输入的统一错误。所有归一化函数都必须拒绝空值。
var errEmpty = errors.New("值为空")

// Normalize 按 kind 归一化原始值，返回可直接写入存储的身份。
//
// 不同 kind 的归一化规则：
//
//   - root_domain / subdomain：见 NormalizeDomain（小写、去结尾点、校验标签）；
//   - ip：见 NormalizeIP（IP 规范化，IPv6 用压缩形式）；
//   - service：见 NormalizeService（主机:端口，端口 1..65535）；
//   - app：仅去首尾空白，保留大小写（应用名与 bundle id 大小写敏感）；
//   - endpoint：见 NormalizeURL（显式 http/https、去默认端口、去 fragment）。
//
// kind 非法或值非法时返回中文错误，调用方应把错误当作“该条数据不可写入”处理，
// 而不是忽略后写入原始值。
func Normalize(kind Kind, raw string) (Identity, error) {
	if !kind.Valid() {
		return Identity{}, fmt.Errorf("未知的资产类型：%q", string(kind))
	}

	var (
		key string
		err error
	)
	switch kind {
	case KindRootDomain, KindSubdomain:
		key, err = NormalizeDomain(raw)
	case KindIP:
		key, err = NormalizeIP(raw)
	case KindService:
		key, err = NormalizeService(raw)
	case KindApp:
		key, err = normalizeApp(raw)
	case KindEndpoint:
		key, err = NormalizeURL(raw)
	}
	if err != nil {
		return Identity{}, fmt.Errorf("%s归一化失败：%w", kind.String(), err)
	}
	return Identity{Kind: kind, Key: key}, nil
}

// NormalizeDomain 归一化域名（根域与子域共用）：去首尾空白、转小写、
// 去掉结尾的点，并校验其为合法的 DNS 主机名。
//
// 接受的规则：至少一个标签，标签只含字母、数字与连字符，标签不以连字符
// 开头或结尾，单标签不超过 63 字节，总长不超过 253 字节。单标签主机名
// （如内网的 intranet）是合法的；但纯数字的顶级标签（如 1.2.3）会被拒绝，
// 因为它更像 IP 或非法输入。
//
// 拒绝的典型输入：空值、IP 字面量（应使用 ip 类型）、含端口（应使用
// service 类型）、含路径或查询（应使用 endpoint 类型）、含下划线或空格、
// 通配符 *。域名大小写不敏感，因此 Example.COM. 与 example.com 得到同一个键。
func NormalizeDomain(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errEmpty
	}
	s = strings.ToLower(s)
	// 结尾的点表示“绝对域名”，与不带点的形式等价；允许连续多个结尾点
	// 这种明显的书写错误一并收敛。
	for strings.HasSuffix(s, ".") {
		s = strings.TrimSuffix(s, ".")
	}
	if s == "" {
		return "", errors.New("域名只有结尾的点")
	}
	if len(s) > maxDomainLen {
		return "", fmt.Errorf("域名过长：%d 字节，上限 %d", len(s), maxDomainLen)
	}
	if net.ParseIP(s) != nil {
		return "", fmt.Errorf("看起来是 IP 地址（%q），请使用 ip 类型", raw)
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("域名不得包含协议（%q），请使用 endpoint 类型", raw)
	}
	if strings.Contains(s, ":") {
		return "", fmt.Errorf("域名不得包含端口（%q），请使用 service 类型", raw)
	}
	if strings.ContainsAny(s, "/?#@ ") {
		return "", fmt.Errorf("域名不得包含路径、查询、用户信息或空格（%q）", raw)
	}

	labels := strings.Split(s, ".")
	for _, label := range labels {
		if label == "" {
			return "", fmt.Errorf("域名包含空标签（%q）", raw)
		}
		if len(label) > maxLabelLen {
			return "", fmt.Errorf("域名标签过长：%q 有 %d 字节，上限 %d", label, len(label), maxLabelLen)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("域名标签不得以连字符开头或结尾：%q", label)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return "", fmt.Errorf("域名标签含非法字符 %q（只允许字母、数字、连字符）：%q", string(c), label)
		}
	}
	if allDigits(labels[len(labels)-1]) {
		return "", fmt.Errorf("域名顶级标签不得为纯数字（%q）", raw)
	}
	return s, nil
}

// NormalizeIP 归一化单个 IP 地址：去首尾空白，用标准形式输出。
//
// IPv4 输出点分十进制；IPv6 输出 RFC 5952 压缩形式（2001:0db8::0001 →
// 2001:db8::1）；IPv4 映射的 IPv6 地址（::ffff:1.2.3.4）会被规范化为 IPv4，
// 因为它们指向同一个主机。不接受 CIDR（请用 NormalizeCIDR）、端口、
// 方括号或 IPv6 zone 后缀 —— 这些都不是 IP 身份的一部分。
func NormalizeIP(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errEmpty
	}
	if strings.Contains(s, "/") {
		return "", fmt.Errorf("IP 值不得包含前缀长度（%q 看起来是 CIDR），请使用 NormalizeCIDR", raw)
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "", fmt.Errorf("无效的 IP 地址：%q", raw)
	}
	return ip.String(), nil
}

// NormalizeCIDR 归一化 CIDR 网段：去首尾空白，并抹掉主机位（10.0.0.5/24
// 与 10.0.0.0/24 得到同一个键），IPv6 用压缩形式。
//
// 必须显式包含前缀长度；裸 IP 会被拒绝，因为它的含义是“单主机”，
// 应由 ip 类型表达。
func NormalizeCIDR(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errEmpty
	}
	if !strings.Contains(s, "/") {
		return "", fmt.Errorf("CIDR 必须包含前缀长度（例如 10.0.0.0/24）：%q", raw)
	}
	if len(s) > maxURLLen {
		return "", fmt.Errorf("CIDR 过长：%d 字节", len(s))
	}
	_, network, err := net.ParseCIDR(s)
	if err != nil {
		return "", fmt.Errorf("无效的 CIDR %q：%v", raw, err)
	}
	return network.String(), nil
}

// NormalizeURL 归一化端点 URL，得到可作为资产身份的字符串。
//
// 规则：
//   - 必须显式包含 http 或 https（缺省协议不会被补全，直接报错）；
//   - scheme 与主机名转小写；
//   - 去掉默认端口（http:80、https:443 视为缺省，从键里删除；其它端口保留）；
//   - 去掉 fragment（# 之后的部分不参与服务器侧寻址）；
//   - 保留 path 与 query，query 原样保留（不做参数排序或重编码）；
//   - 空路径与“/”等价，统一输出为不带结尾斜杠的形式，保证
//     http://example.com 与 http://example.com/ 是同一个端点；
//   - 去掉 userinfo：凭据不属于资产身份，且不应进入唯一索引。
//
// 主机可以是域名或 IP 字面量，二者分别按 NormalizeDomain / NormalizeIP 校验；
// IPv6 主机在输出时保留方括号。路径以解码后的形式为准并重新做标准转义，
// 因此 /a%2Fb 与 /a/b、/%7euser 与 /~user 会被视为同一端点。
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errEmpty
	}
	if len(s) > maxURLLen {
		return "", fmt.Errorf("URL 过长：%d 字节，上限 %d", len(s), maxURLLen)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("无效的 URL %q：%v", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return "", fmt.Errorf("URL 必须显式包含 http/https 协议：%q", raw)
	}
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("端点只支持 http/https，收到 %q：%q", u.Scheme, raw)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("不支持不透明（opaque）URL：%q", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("URL 不得包含用户信息（userinfo）：%q", raw)
	}

	rawHost := u.Hostname()
	if rawHost == "" {
		return "", fmt.Errorf("URL 缺少主机名：%q", raw)
	}
	var host string
	if ip := net.ParseIP(rawHost); ip != nil {
		host = ip.String()
	} else {
		host, err = NormalizeDomain(rawHost)
		if err != nil {
			return "", fmt.Errorf("URL 主机非法：%w", err)
		}
	}

	port := u.Port()
	if port != "" {
		p, perr := parsePort(port)
		if perr != nil {
			return "", fmt.Errorf("URL 端口非法：%w", perr)
		}
		if (scheme == "http" && p == 80) || (scheme == "https" && p == 443) {
			port = ""
		} else {
			port = strconv.Itoa(p)
		}
	}

	u.Scheme = scheme
	u.Host = joinHostPort(host, port)
	u.Fragment = ""
	u.RawFragment = ""
	// 路径以解码后的形式重新转义，抹掉“同一路径的不同编码”带来的身份差异。
	u.RawPath = ""
	if u.Path == "/" {
		u.Path = ""
	}
	// 没有 query 时不要输出孤立的“?”。
	if u.RawQuery == "" {
		u.ForceQuery = false
	}
	return u.String(), nil
}

// NormalizeService 归一化“主机:端口”形式的服务身份。
//
// 必须是显式的 host:port；端口为十进制 1..65535（前导零会被去掉，例如
// example.com:0443 → example.com:443）；主机按 IP 或域名规则归一化。
// IPv6 主机必须用方括号书写（[::1]:443），输出同样保留方括号，避免与
// 端口分隔符产生歧义。
func NormalizeService(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errEmpty
	}
	if len(s) > maxServiceLen {
		return "", fmt.Errorf("服务地址过长：%d 字节", len(s))
	}
	if !strings.Contains(s, ":") {
		return "", fmt.Errorf("服务地址必须是“主机:端口”形式：%q", raw)
	}
	rawHost, rawPort, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("服务地址必须是“主机:端口”形式（IPv6 请用方括号）：%q", raw)
	}
	if rawHost == "" {
		return "", fmt.Errorf("服务地址缺少主机：%q", raw)
	}
	port, err := parsePort(rawPort)
	if err != nil {
		return "", err
	}

	var host string
	if ip := net.ParseIP(rawHost); ip != nil {
		host = ip.String()
	} else {
		host, err = NormalizeDomain(rawHost)
		if err != nil {
			return "", fmt.Errorf("服务主机非法：%w", err)
		}
	}
	return joinHostPort(host, strconv.Itoa(port)), nil
}

// normalizeApp 归一化应用身份：仅去首尾空白。
//
// 应用名与 bundle id 大小写敏感（com.Example.App 与 com.example.app 可能
// 是两个应用），因此不做大小写折叠，也不做其它替换。空值报错。
func normalizeApp(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errEmpty
	}
	if len(s) > maxAppLen {
		return "", fmt.Errorf("应用名过长：%d 字节，上限 %d", len(s), maxAppLen)
	}
	return s, nil
}

// joinHostPort 拼接主机与端口；IPv6 主机加方括号。端口为空时只输出主机。
func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// parsePort 解析端口：只接受十进制数字，范围 1..65535。
func parsePort(raw string) (int, error) {
	if raw == "" {
		return 0, errors.New("端口为空")
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, fmt.Errorf("端口必须是十进制数字：%q", raw)
		}
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("端口非法：%q", raw)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("端口超出范围 1..65535：%d", port)
	}
	return port, nil
}

// allDigits 报告 s 是否非空且全部由数字组成。
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
