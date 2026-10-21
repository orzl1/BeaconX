package policy

// 内置默认规则的名称（中文）。对外通过 Rule.Name 暴露，测试与审计按名称引用。
const (
	// DefaultRuleNameAllowSafeHTTPMethod 放行只读的 GET/HEAD 探测。
	DefaultRuleNameAllowSafeHTTPMethod = "允许 GET/HEAD 请求"
	// DefaultRuleNameDenyDownloadAndExec 拒绝"下载脚本并通过管道交给 shell 执行"。
	DefaultRuleNameDenyDownloadAndExec = "禁止下载并执行"
	// DefaultRuleNameDenyRemoveRoot 拒绝删除根目录。
	DefaultRuleNameDenyRemoveRoot = "禁止删除根目录"
	// DefaultRuleNameAskShell 兜底：所有 shell 调用默认人工审批。
	DefaultRuleNameAskShell = "shell 需人工审批"
	// DefaultRuleNameAskOtherHTTPMethod 兜底：非 GET/HEAD 的 HTTP 调用默认人工审批。
	DefaultRuleNameAskOtherHTTPMethod = "HTTP 其他方法需审批"
)

// 内置默认规则的匹配模式。
//
// 注意：匹配对象是 args 的**原始 JSON 文本**，不是解析后的字段，因此这些正则
// 里出现的 \" 对应的是 JSON 序列化后的引号；参数顺序/空白变化会影响匹配。默认
// 规则只覆盖最常见、最危险的形态，不追求完备（真正的完备性要靠审批与审计）。
const (
	defaultPatternSafeHTTPMethod  = `\"method\"\s*:\s*\"(GET|HEAD)\"`
	defaultPatternDownloadAndExec = `(curl|wget)[^"]*\|[^"]*(sh|bash)`
	defaultPatternRemoveRoot      = `rm\s+-rf?\s+/(\s|\"|$)`
)

// 默认规则的默认审批超时（秒）。
const defaultAskTimeoutSeconds = 60

// SeedDefaults 返回内置的默认策略规则集（每次调用返回新的切片，调用方可自由
// 修改/持久化，不影响后续调用）。
//
// 规则取舍：
//
//   - 明确的底线用 deny + immutable（下载并执行、删除根目录），priority 200，
//     即使上层后来加了 allow 规则也无法覆盖（priority 更高或同优先级更靠前）；
//   - 只读探测（GET/HEAD）用 allow + immutable，priority 100：Agent 的侦察
//     动作不应每一步都要人工确认；
//   - 其余一律 ask（priority 50）：shell 一律人工审批、非 GET/HEAD 的 HTTP
//     方法也人工审批。默认询问而不是默认放行，是"策略缺失即人工确认"的体现；
//   - 两条 ask 规则带 60s 超时；超时后如何处理（拒绝/挂起）由上层决定，领域层
//     只携带 TimeoutSeconds。
//
// 返回的规则均 Enabled=true，且都通过 ValidateRules（见测试）。Immutable 只是
// 标记，是否禁止删除/修改由存储层与 API 层决定。
func SeedDefaults() []Rule {
	return []Rule{
		{
			Name:       DefaultRuleNameAllowSafeHTTPMethod,
			Tool:       "http_probe",
			ArgPattern: defaultPatternSafeHTTPMethod,
			Action:     Allow,
			Priority:   100,
			Enabled:    true,
			Immutable:  true,
		},
		{
			Name:       DefaultRuleNameDenyDownloadAndExec,
			Tool:       "shell_exec",
			ArgPattern: defaultPatternDownloadAndExec,
			Action:     Deny,
			Priority:   200,
			Enabled:    true,
			Immutable:  true,
		},
		{
			Name:       DefaultRuleNameDenyRemoveRoot,
			Tool:       "shell_exec",
			ArgPattern: defaultPatternRemoveRoot,
			Action:     Deny,
			Priority:   200,
			Enabled:    true,
			Immutable:  true,
		},
		{
			Name:           DefaultRuleNameAskShell,
			Tool:           "shell_exec",
			Action:         Ask,
			Priority:       50,
			TimeoutSeconds: defaultAskTimeoutSeconds,
			Enabled:        true,
		},
		{
			Name:           DefaultRuleNameAskOtherHTTPMethod,
			Tool:           "http_probe",
			Action:         Ask,
			Priority:       50,
			TimeoutSeconds: defaultAskTimeoutSeconds,
			Enabled:        true,
		},
	}
}
