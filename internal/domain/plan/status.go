package plan

// StepStatus 是步骤状态。取值即存储层字面量，不要随意改动。
type StepStatus string

const (
	// StepPending 表示步骤尚未开始，是 ReadySteps 唯一认可的"可派发"状态。
	StepPending StepStatus = "pending"
	// StepReady 表示步骤已满足依赖、等待执行器领取（预留状态，语义上等价于
	// pending 的下一拍；是否使用由调度层决定）。
	StepReady StepStatus = "ready"
	// StepRunning 表示步骤正在执行。
	StepRunning StepStatus = "running"
	// StepDone 表示步骤成功完成，是依赖方就绪的唯一满足条件。
	StepDone StepStatus = "done"
	// StepSkipped 表示步骤被跳过（例如前置结论使其失去意义），属于终态但
	// 不满足依赖——依赖方仍然不会就绪。
	StepSkipped StepStatus = "skipped"
	// StepFailed 表示步骤失败，属于终态，依赖方不会就绪。
	StepFailed StepStatus = "failed"
)

// Valid 报告 s 是否是已知的步骤状态。空白字符串与未知取值返回 false。
func (s StepStatus) Valid() bool {
	switch s {
	case StepPending, StepReady, StepRunning, StepDone, StepSkipped, StepFailed:
		return true
	}
	return false
}

// Terminal 报告 s 是否是终态。终态步骤不会再被调度器推进。
//
// 终态包括 done / skipped / failed；pending / ready / running 都不是终态，
// 空白或未知状态同样不是终态（保守：调用方应先用 Valid 判断）。
func (s StepStatus) Terminal() bool {
	switch s {
	case StepDone, StepSkipped, StepFailed:
		return true
	}
	return false
}

// String 返回状态的中文名称，仅用于日志与面向人的错误信息；未知状态回退为
// 原值，便于排障时看到模型/数据库里的真实取值。
func (s StepStatus) String() string {
	switch s {
	case StepPending:
		return "待执行"
	case StepReady:
		return "可执行"
	case StepRunning:
		return "执行中"
	case StepDone:
		return "已完成"
	case StepSkipped:
		return "已跳过"
	case StepFailed:
		return "已失败"
	}
	return string(s)
}
