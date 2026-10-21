package policy

// Action 是策略裁决结果。取值即存储层字面量，不要随意改动。
type Action string

const (
	// Allow 表示直接放行，无需人工介入。
	Allow Action = "allow"
	// Ask 表示需要人工审批（或上层配置的审批通道）后才能执行。
	Ask Action = "ask"
	// Deny 表示直接拒绝执行。
	Deny Action = "deny"
)

// Valid 报告 a 是否是已知的裁决动作。空白与未知取值返回 false。
func (a Action) Valid() bool {
	switch a {
	case Allow, Ask, Deny:
		return true
	}
	return false
}

// String 返回裁决动作的中文名称，用于日志、审批界面与 Decision.Reason；
// 未知动作回退为原值，便于排障时看到真实取值。
func (a Action) String() string {
	switch a {
	case Allow:
		return "允许"
	case Ask:
		return "询问"
	case Deny:
		return "拒绝"
	}
	return string(a)
}

// Risk 是动作的风险等级，供审批界面展示、审计与后续"按风险路由"使用。
// 规则本身不携带 Risk：风险由工具/参数在审批时评估，避免把静态标签当成安全结论。
type Risk string

const (
	// RiskLow 表示低风险（只读、无副作用或影响可忽略）。
	RiskLow Risk = "low"
	// RiskMedium 表示中风险（可能改变目标状态，但影响可控）。
	RiskMedium Risk = "medium"
	// RiskHigh 表示高风险（破坏性、外带数据、权限提升等）。
	RiskHigh Risk = "high"
)

// Valid 报告 r 是否是已知的风险等级。
func (r Risk) Valid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh:
		return true
	}
	return false
}

// String 返回风险等级的中文名称；未知等级回退为原值。
func (r Risk) String() string {
	switch r {
	case RiskLow:
		return "低"
	case RiskMedium:
		return "中"
	case RiskHigh:
		return "高"
	}
	return string(r)
}
