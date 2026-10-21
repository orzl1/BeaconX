// Package task 定义任务状态机：任务状态、事件、副作用，以及把它们折叠成
// 新状态的纯函数 Reduce。
//
// 设计原则：
//
//   - **纯函数**：Reduce 只根据「当前状态 + 事件」计算下一状态，不读不写任何
//     外部状态（数据库、时钟、日志），因此可以被穷举测试、可以回放事件流、
//     可以在事务里先算后写；
//   - **显式副作用**：状态变化需要联动的东西（拉起 / 停止 runner）以 Effect
//     返回，由引擎执行。领域层不直接操作 goroutine 或进程；
//   - **默认拒绝**：迁移表里没有的组合一律报错，而不是静默保持原状态。状态机
//     的价值在于把「不该发生的事」变成显式错误，而不是把它藏起来。
//
// 状态与合法迁移（空白状态表示任务尚未物化，是事件回放的起点）：
//
//	当前状态   事件                      下一状态    副作用
//	（空）     created                   queued      —
//	queued     admitted                  running     start_runners
//	queued     cancelled / deleted       cancelled   —
//	queued     recovered                 queued      —
//	running    paused                    paused      stop_runners
//	running    finalizing                finalizing  stop_runners
//	running    failed                    failed      stop_runners
//	running    cancelled / deleted       cancelled   stop_runners
//	running    recovered                 queued      stop_runners
//	paused     resumed                   queued      —
//	paused     cancelled / deleted       cancelled   —
//	finalizing completed                 done        —
//	finalizing failed                    failed      —
//	finalizing cancelled / deleted       cancelled   —
//	done / failed / cancelled  任何事件               报错（终态不可再变）
//
// 其余组合（包括未列出的合法事件）一律返回 ErrIllegalTransition。
//
// 本包是纯领域逻辑：只使用标准库，不依赖数据库与其它内部包。
package task

import (
	"errors"
	"fmt"
	"time"
)

// Status 是任务状态。取值即存储层字面量，不要随意改动。
type Status string

const (
	// StatusQueued 表示任务已创建、等待准入（排队中，未占用执行资源）。
	StatusQueued Status = "queued"
	// StatusRunning 表示任务已准入并正在推进（runner 存活，会领新动作）。
	StatusRunning Status = "running"
	// StatusPaused 表示任务被人为暂停：runner 已停止，但可以恢复。
	StatusPaused Status = "paused"
	// StatusFinalizing 表示任务进入收尾：不再领新动作，只等收尾动作与总结完成。
	StatusFinalizing Status = "finalizing"
	// StatusDone 表示任务正常完成（终态）。
	StatusDone Status = "done"
	// StatusFailed 表示任务失败终止（终态）。
	StatusFailed Status = "failed"
	// StatusCancelled 表示任务被取消或删除（终态）。取消与删除共用该状态，
	// 两者的区别由事件的 Reason 保留。
	StatusCancelled Status = "cancelled"
)

// statusUnset 是「任务尚未物化」的哨兵状态：事件回放的第一条 created 事件
// 从它开始。它不是一个可持久化的业务状态，因此 Valid 对它返回 false。
const statusUnset Status = ""

// Valid 报告 s 是否是已知的业务状态。
//
// 注意：空白状态（""）是 Reduce 的起点哨兵，不属于业务状态，返回 false。
func (s Status) Valid() bool {
	switch s {
	case StatusQueued, StatusRunning, StatusPaused, StatusFinalizing,
		StatusDone, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Terminal 报告 s 是否是终态：终态任务不再接受任何事件。
//
// 只有 done / failed / cancelled 是终态；finalizing 不是终态（还要走向
// done / failed / cancelled）。未知状态与空白状态一律返回 false。
func (s Status) Terminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// String 返回状态的中文字称，用于错误信息与日志。
func (s Status) String() string {
	switch s {
	case StatusQueued:
		return "排队中"
	case StatusRunning:
		return "运行中"
	case StatusPaused:
		return "已暂停"
	case StatusFinalizing:
		return "收尾中"
	case StatusDone:
		return "已完成"
	case StatusFailed:
		return "已失败"
	case StatusCancelled:
		return "已取消"
	}
	return string(s)
}

// EventType 是驱动状态机的事件类型。取值即存储层/审计日志字面量。
type EventType string

const (
	// EvCreated 表示任务被创建（只从空白状态被接受）。
	EvCreated EventType = "created"
	// EvAdmitted 表示任务通过准入，开始占用执行资源。
	EvAdmitted EventType = "admitted"
	// EvStarted 表示 runner 已实际启动。
	//
	// 注意：当前迁移表没有任何规则消费它，因此 Reduce 对它会返回
	// ErrIllegalTransition。保留该取值是为了与审计事件表对齐，语义待定
	// （见包文档与任务拆解记录）。
	EvStarted EventType = "started"
	// EvPaused 表示人为暂停。
	EvPaused EventType = "paused"
	// EvResumed 表示从暂停恢复：回到排队，等待重新准入。
	EvResumed EventType = "resumed"
	// EvFinalizing 表示进入收尾：停止领取新动作。
	EvFinalizing EventType = "finalizing"
	// EvCompleted 表示收尾完成，任务成功结束。
	EvCompleted EventType = "completed"
	// EvFailed 表示任务失败终止。
	EvFailed EventType = "failed"
	// EvCancelled 表示人为取消。
	EvCancelled EventType = "cancelled"
	// EvDeleted 表示用户删除任务；与取消一样落入 cancelled 终态。
	EvDeleted EventType = "deleted"
	// EvRecovered 表示进程重启后的恢复对账：把「以为在跑」的任务退回排队，
	// 由引擎重新准入，避免状态与实际 runner 长期不一致。
	EvRecovered EventType = "recovered"
)

// valid 报告 t 是否是已知的事件类型。
func (t EventType) valid() bool {
	switch t {
	case EvCreated, EvAdmitted, EvStarted, EvPaused, EvResumed, EvFinalizing,
		EvCompleted, EvFailed, EvCancelled, EvDeleted, EvRecovered:
		return true
	}
	return false
}

// String 返回事件类型的中文字称，用于错误信息与日志。
func (t EventType) String() string {
	switch t {
	case EvCreated:
		return "创建"
	case EvAdmitted:
		return "准入"
	case EvStarted:
		return "启动"
	case EvPaused:
		return "暂停"
	case EvResumed:
		return "恢复"
	case EvFinalizing:
		return "进入收尾"
	case EvCompleted:
		return "完成"
	case EvFailed:
		return "失败"
	case EvCancelled:
		return "取消"
	case EvDeleted:
		return "删除"
	case EvRecovered:
		return "崩溃恢复"
	}
	return string(t)
}

// Event 是一条任务事件。Reduce 只消费 Type；Reason 与 At 供审计、日志与
// 幂等去重使用，状态机本身不读取、也不修改它们。
type Event struct {
	Type   EventType
	Reason string
	At     time.Time
}

// Valid 只校验事件类型是否合法（不关心当前状态与迁移是否成立）。
//
// Reason 与 At 不参与校验：它们可以为空/零值，由调用方按需填充。
func (e Event) Valid() bool {
	return e.Type.valid()
}

// Effect 是状态迁移需要引擎执行的副作用。领域层只声明「要做什么」，
// 不负责怎么做，因此同一份状态机逻辑可以用于真实引擎、回放与测试。
type Effect string

const (
	// EffectStartRunners 表示拉起任务执行器（让任务开始领动作）。
	EffectStartRunners Effect = "start_runners"
	// EffectStopRunners 表示停止任务执行器（不再领动作，必要时等收尾）。
	EffectStopRunners Effect = "stop_runners"
)

// String 返回副作用的中文字称，用于错误信息与日志。
func (e Effect) String() string {
	switch e {
	case EffectStartRunners:
		return "启动执行器"
	case EffectStopRunners:
		return "停止执行器"
	}
	return string(e)
}

// 状态机的错误分类。调用方用 errors.Is 判断即可，不需要解析错误文本：
//
//   - ErrInvalidStatus：当前状态不是已知业务状态，也不是空白哨兵；
//   - ErrInvalidEvent：事件类型未知；
//   - ErrTerminal：任务已处于终态，任何事件都不再接受；
//   - ErrIllegalTransition：状态与事件都合法，但迁移表里没有这条边。
var (
	// ErrInvalidStatus 表示当前状态取值非法。
	ErrInvalidStatus = errors.New("未知的任务状态")
	// ErrInvalidEvent 表示事件类型取值非法。
	ErrInvalidEvent = errors.New("未知的任务事件")
	// ErrTerminal 表示任务已处于终态，不能再接收事件。
	ErrTerminal = errors.New("任务已处于终态，不再接受事件")
	// ErrIllegalTransition 表示迁移表不允许这条边。
	ErrIllegalTransition = errors.New("非法的状态迁移")
)

// Reduce 是纯函数：给定当前状态 prev 与事件 ev，返回下一状态与需要执行的
// 副作用；非法迁移返回 error。
//
// 约定：
//
//   - prev 为空字符串表示「任务尚未物化」，只接受 created 事件，产出 queued；
//   - 返回 error 时，第一个返回值恒为 prev（状态未变更），副作用恒为 nil，
//     调用方可以直接跳过写库与副作用执行；
//   - 成功时返回的 effects 可能是 nil（无副作用），调用方按 len 判断即可；
//   - 返回值中的 effects 是本次调用新分配的切片，调用方可以安全修改；
//   - 函数不读取时钟、不读全局变量、不修改入参，因此同一输入永远得到同一输出。
func Reduce(prev Status, ev Event) (Status, []Effect, error) {
	if prev != statusUnset && !prev.Valid() {
		return prev, nil, fmt.Errorf("%w：%q", ErrInvalidStatus, string(prev))
	}
	if !ev.Valid() {
		return prev, nil, fmt.Errorf("%w：%q", ErrInvalidEvent, string(ev.Type))
	}
	if prev.Terminal() {
		return prev, nil, fmt.Errorf("%w：当前为「%s」，收到「%s」", ErrTerminal, prev, ev.Type)
	}

	switch prev {
	case statusUnset:
		if ev.Type == EvCreated {
			return StatusQueued, nil, nil
		}

	case StatusQueued:
		switch ev.Type {
		case EvAdmitted:
			return StatusRunning, []Effect{EffectStartRunners}, nil
		case EvCancelled, EvDeleted:
			// 还没占用执行资源，不需要停 runner。
			return StatusCancelled, nil, nil
		case EvRecovered:
			// 已经是排队态：恢复对账是幂等的空操作（不重复产生副作用）。
			return StatusQueued, nil, nil
		}

	case StatusRunning:
		switch ev.Type {
		case EvPaused:
			return StatusPaused, []Effect{EffectStopRunners}, nil
		case EvFinalizing:
			return StatusFinalizing, []Effect{EffectStopRunners}, nil
		case EvFailed:
			return StatusFailed, []Effect{EffectStopRunners}, nil
		case EvCancelled, EvDeleted:
			return StatusCancelled, []Effect{EffectStopRunners}, nil
		case EvRecovered:
			// runner 已随进程一起消失，状态退回排队等重新准入。
			return StatusQueued, []Effect{EffectStopRunners}, nil
		}

	case StatusPaused:
		switch ev.Type {
		case EvResumed:
			// 恢复后先回排队，由准入重新决定何时开跑（不直接 running）。
			return StatusQueued, nil, nil
		case EvCancelled, EvDeleted:
			// runner 早已停止，无需副作用。
			return StatusCancelled, nil, nil
		}

	case StatusFinalizing:
		switch ev.Type {
		case EvCompleted:
			return StatusDone, nil, nil
		case EvFailed:
			return StatusFailed, nil, nil
		case EvCancelled, EvDeleted:
			return StatusCancelled, nil, nil
		}
	}

	return prev, nil, fmt.Errorf("%w：「%s」状态下不接受「%s」事件", ErrIllegalTransition, prev, ev.Type)
}
