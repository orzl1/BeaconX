package finding

import (
	"errors"
	"fmt"
)

// RetestStatus 是复测状态。取值即存储层字面量，不要随意改动。
type RetestStatus string

const (
	// RetestPending 表示已发起、尚未开始（终态之前的唯一入口）。
	RetestPending RetestStatus = "pending"
	// RetestRunning 表示复测执行中。
	RetestRunning RetestStatus = "running"
	// RetestPassed 表示复测通过（原问题仍可复现或修复验证未通过，由调用方语义决定）。
	RetestPassed RetestStatus = "passed"
	// RetestFailed 表示复测失败（结论为未通过/未复现）。
	RetestFailed RetestStatus = "failed"
	// RetestError 表示复测因环境或执行错误而中止（终态）。
	RetestError RetestStatus = "error"
)

// Valid 报告 s 是否是已知的复测状态。
func (s RetestStatus) Valid() bool {
	switch s {
	case RetestPending, RetestRunning, RetestPassed, RetestFailed, RetestError:
		return true
	}
	return false
}

// Terminal 报告 s 是否是终态。终态复测不再接受任何状态迁移。
func (s RetestStatus) Terminal() bool {
	switch s {
	case RetestPassed, RetestFailed, RetestError:
		return true
	}
	return false
}

// String 返回复测状态的中文名称，仅用于日志与面向人的错误信息。
func (s RetestStatus) String() string {
	switch s {
	case RetestPending:
		return "待开始"
	case RetestRunning:
		return "执行中"
	case RetestPassed:
		return "已通过"
	case RetestFailed:
		return "未通过"
	case RetestError:
		return "执行错误"
	}
	return string(s)
}

// 复测状态机的错误分类。调用方用 errors.Is 判断即可。
var (
	// ErrInvalidRetestStatus 表示复测状态取值非法。
	ErrInvalidRetestStatus = errors.New("未知的复测状态")
	// ErrIllegalRetestTransition 表示迁移表不允许这条边（含同状态）。
	ErrIllegalRetestTransition = errors.New("非法的复测状态迁移")
	// ErrRetestTerminal 表示复测已处于终态，不能再迁移。
	ErrRetestTerminal = errors.New("复测已处于终态，不能再迁移")
)

// ValidateRetestTransition 校验一次复测状态迁移。
//
// 合法迁移：
//
//	pending → running
//	running → passed / failed / error
//
// 约定与发现状态机一致：
//   - from/to 任一未知 → ErrInvalidRetestStatus；
//   - from == to → ErrIllegalRetestTransition（同状态不算迁移）；
//   - from 为终态 → ErrRetestTerminal；
//   - 其余未列出的组合 → ErrIllegalRetestTransition。
func ValidateRetestTransition(from, to RetestStatus) error {
	if !from.Valid() {
		return fmt.Errorf("%w：%q", ErrInvalidRetestStatus, string(from))
	}
	if !to.Valid() {
		return fmt.Errorf("%w：%q", ErrInvalidRetestStatus, string(to))
	}
	if from == to {
		return fmt.Errorf("%w：%s→%s 状态未变化", ErrIllegalRetestTransition, from, to)
	}
	if from.Terminal() {
		return fmt.Errorf("%w：当前为「%s」", ErrRetestTerminal, from)
	}

	switch from {
	case RetestPending:
		if to == RetestRunning {
			return nil
		}
	case RetestRunning:
		switch to {
		case RetestPassed, RetestFailed, RetestError:
			return nil
		}
	}
	return fmt.Errorf("%w：「%s」不能迁移到「%s」", ErrIllegalRetestTransition, from, to)
}
