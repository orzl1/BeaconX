package plan

// 决策规模上限。上限既约束模型输出，也避免单次决策写库过大。
const (
	// MaxSteps 是单次决策允许声明的步骤数上限。
	MaxSteps = 20
	// MaxActions 是单次决策允许携带的动作数上限。
	MaxActions = 10
	// MaxStepKeyLen 是步骤 key 的字节长度上限。key 只允许 ASCII，所以
	// 字节长度与字符长度一致。
	MaxStepKeyLen = 64
)

// 动作类型。Action.Kind 必须是这四个取值之一，不做大小写归一（规划输出应当
// 是规范化的；宽容归一化会把模型错误藏起来）。
const (
	// ActionKindProbe 表示探测类动作（收集信息，通常无副作用）。
	ActionKindProbe = "probe"
	// ActionKindVerify 表示验证类动作（确认假设 / 复现结论）。
	ActionKindVerify = "verify"
	// ActionKindReport 表示报告类动作（登记结论 / 发现）。
	ActionKindReport = "report"
	// ActionKindOther 表示未归类的动作。
	ActionKindOther = "other"
)

// Step 是任务内的一个执行步骤。Key 在任务内唯一，是依赖关系与动作引用的锚点。
type Step struct {
	// Key 是步骤标识：只允许小写字母、数字、下划线、连字符，长度 1..64，
	// 例如 "recon"、"web-enum"、"verify_admin"。
	Key string `json:"key"`
	// Title 是面向人的简短标题。领域层不强制非空（规划器可能先给 key 再补
	// 标题），是否展示由上层决定。
	Title string `json:"title"`
	// Detail 是可选的补充说明。
	Detail string `json:"detail,omitempty"`
	// DependsOn 是本步骤依赖的其它步骤 key。必须能在本次决策的步骤或调用方
	// 提供的已知步骤集合中找到；不允许自依赖与重复。
	DependsOn []string `json:"depends_on,omitempty"`
}

// Action 是规划器要求执行的一次具体动作，必须挂在某个步骤上。
type Action struct {
	// StepKey 是动作所属步骤的 key，必填。
	StepKey string `json:"step_key"`
	// Kind 是动作类型：probe / verify / report / other，必填。
	Kind string `json:"kind"`
	// Target 是可选的执行目标（域名、URL、资产 id 等），具体解释由执行层决定。
	Target string `json:"target,omitempty"`
	// Detail 是可选的执行细节（命令意图、验证点等）。
	Detail string `json:"detail,omitempty"`
}

// Decision 是一次规划产出的完整决策。
//
// 它同时承载两种信息：
//
//   - 计划增量：新声明的步骤（Steps）与本次要执行的动作（Actions）；
//   - 结束判断：Done 表示规划器认为任务目标已达成，Reason 给出理由。
//
// Done 为 true 时通常不再携带动作，但领域层不强制这一点（收尾动作与最终
// 报告可能需要同时存在），由调度层决定。
type Decision struct {
	// Summary 是这次决策的一句话概述，可为空。
	Summary string `json:"summary,omitempty"`
	// Steps 是本次新增（或重新声明）的步骤。
	Steps []Step `json:"steps,omitempty"`
	// Actions 是本次要执行的动作。
	Actions []Action `json:"actions,omitempty"`
	// Done 表示规划器判定任务目标已达成。
	Done bool `json:"done"`
	// Reason 是 Done 的理由或本次决策的说明，可为空。
	Reason string `json:"reason,omitempty"`
}

// validActionKind 报告 kind 是否是四个合法动作类型之一。
func validActionKind(kind string) bool {
	switch kind {
	case ActionKindProbe, ActionKindVerify, ActionKindReport, ActionKindOther:
		return true
	}
	return false
}
