// Package finding 定义「发现」与「复测」的领域模型：严重级别、处置状态、
// 复测状态，以及它们的状态机校验。
//
// 设计原则：
//
//   - **纯函数**：不读时钟、不访问数据库、不依赖其它内部包，因此可以被穷举测试，
//     也可以在事务里「先算后写」；
//   - **默认拒绝**：状态迁移表里没有的组合一律报错，而不是静默保持原状态；
//   - **同状态不算迁移**：open→open 也报错，避免调用方把「重复提交」误当成
//     状态推进（幂等是调用方的语义，不是状态机的语义）；
//   - **终态不可再变**：closed 与 false_positive 是终态，任何迁移都拒绝。
package finding

import (
	"errors"
	"fmt"
)

// Severity 是发现的严重级别。取值即存储层字面量，不要随意改动。
type Severity string

const (
	// SeverityInfo 表示信息级（无直接安全影响，但值得记录）。
	SeverityInfo Severity = "info"
	// SeverityLow 表示低危。
	SeverityLow Severity = "low"
	// SeverityMedium 表示中危。
	SeverityMedium Severity = "medium"
	// SeverityHigh 表示高危。
	SeverityHigh Severity = "high"
	// SeverityCritical 表示严重。
	SeverityCritical Severity = "critical"
)

// Valid 报告 s 是否是已知的严重级别。
func (s Severity) Valid() bool {
	switch s {
	case SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return true
	}
	return false
}

// String 返回严重级别的中文名称，仅用于日志与面向人的错误信息。
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "信息"
	case SeverityLow:
		return "低危"
	case SeverityMedium:
		return "中危"
	case SeverityHigh:
		return "高危"
	case SeverityCritical:
		return "严重"
	}
	return string(s)
}

// Status 是发现的处置状态。取值即存储层字面量，不要随意改动。
type Status string

const (
	// StatusOpen 表示刚登记、待处理。
	StatusOpen Status = "open"
	// StatusConfirmed 表示已确认（人工或复测确认存在）。
	StatusConfirmed Status = "confirmed"
	// StatusFixed 表示已修复。
	StatusFixed Status = "fixed"
	// StatusRetest 表示等待/正在复测。
	StatusRetest Status = "retest"
	// StatusClosed 表示已关闭（终态）。
	StatusClosed Status = "closed"
	// StatusFalsePositive 表示误报（终态）。
	StatusFalsePositive Status = "false_positive"
)

// Valid 报告 s 是否是已知的处置状态。
func (s Status) Valid() bool {
	switch s {
	case StatusOpen, StatusConfirmed, StatusFixed, StatusRetest, StatusClosed, StatusFalsePositive:
		return true
	}
	return false
}

// Terminal 报告 s 是否是终态。终态发现不再接受任何状态迁移。
func (s Status) Terminal() bool {
	switch s {
	case StatusClosed, StatusFalsePositive:
		return true
	}
	return false
}

// String 返回处置状态的中文名称，仅用于日志与面向人的错误信息。
func (s Status) String() string {
	switch s {
	case StatusOpen:
		return "待处理"
	case StatusConfirmed:
		return "已确认"
	case StatusFixed:
		return "已修复"
	case StatusRetest:
		return "待复测"
	case StatusClosed:
		return "已关闭"
	case StatusFalsePositive:
		return "误报"
	}
	return string(s)
}

// 状态机错误分类。调用方用 errors.Is 判断即可，不需要解析错误文本：
//
//   - ErrInvalidStatus：from 或 to 不是已知状态；
//   - ErrIllegalTransition：状态合法，但迁移表里没有这条边（含同状态）；
//   - ErrTerminalStatus：from 已是终态，不接受任何迁移。
var (
	// ErrInvalidStatus 表示发现状态取值非法。
	ErrInvalidStatus = errors.New("未知的发现状态")
	// ErrIllegalTransition 表示迁移表不允许这条边。
	ErrIllegalTransition = errors.New("非法的发现状态迁移")
	// ErrTerminalStatus 表示发现已处于终态，不能再迁移。
	ErrTerminalStatus = errors.New("发现已处于终态，不能再迁移")
)

// ValidateTransition 校验一次发现状态迁移。
//
// 合法迁移（终态不可再变）：
//
//	open          → confirmed / false_positive / closed
//	confirmed     → fixed / retest / closed
//	retest        → confirmed / fixed / closed
//	fixed         → retest / closed
//	closed        → （终态）
//	false_positive→ （终态）
//
// 约定：
//   - from/to 任一未知 → ErrInvalidStatus；
//   - from == to → ErrIllegalTransition（同状态不算迁移）；
//   - from 为终态 → ErrTerminalStatus；
//   - 其余未列出的组合 → ErrIllegalTransition。
//
// 函数不读时钟、不修改入参，同一输入永远得到同一结果。
func ValidateTransition(from, to Status) error {
	if !from.Valid() {
		return fmt.Errorf("%w：%q", ErrInvalidStatus, string(from))
	}
	if !to.Valid() {
		return fmt.Errorf("%w：%q", ErrInvalidStatus, string(to))
	}
	if from == to {
		return fmt.Errorf("%w：%s→%s 状态未变化", ErrIllegalTransition, from, to)
	}
	if from.Terminal() {
		return fmt.Errorf("%w：当前为「%s」", ErrTerminalStatus, from)
	}

	switch from {
	case StatusOpen:
		switch to {
		case StatusConfirmed, StatusFalsePositive, StatusClosed:
			return nil
		}
	case StatusConfirmed:
		switch to {
		case StatusFixed, StatusRetest, StatusClosed:
			return nil
		}
	case StatusRetest:
		switch to {
		case StatusConfirmed, StatusFixed, StatusClosed:
			return nil
		}
	case StatusFixed:
		switch to {
		case StatusRetest, StatusClosed:
			return nil
		}
	}
	return fmt.Errorf("%w：「%s」不能迁移到「%s」", ErrIllegalTransition, from, to)
}
