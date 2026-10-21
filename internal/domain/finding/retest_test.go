package finding

import (
	"errors"
	"testing"
)

// TestRetestStatusValidAndTerminal 覆盖复测状态枚举与终态判定。
func TestRetestStatusValidAndTerminal(t *testing.T) {
	valid := []RetestStatus{RetestPending, RetestRunning, RetestPassed, RetestFailed, RetestError}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("复测状态 %q 应合法", string(s))
		}
		if s.String() == string(s) {
			t.Errorf("复测状态 %q 缺少中文名称", string(s))
		}
	}
	for _, s := range []RetestStatus{"", "done", "Passed", "running "} {
		if s.Valid() {
			t.Errorf("复测状态 %q 应非法", string(s))
		}
		if s.Terminal() {
			t.Errorf("非法复测状态 %q 不应被判定为终态", string(s))
		}
	}
	for _, s := range []RetestStatus{RetestPassed, RetestFailed, RetestError} {
		if !s.Terminal() {
			t.Errorf("复测状态 %q 应为终态", string(s))
		}
	}
	for _, s := range []RetestStatus{RetestPending, RetestRunning} {
		if s.Terminal() {
			t.Errorf("复测状态 %q 不应为终态", string(s))
		}
	}
}

// TestValidateRetestTransitionMatrix 穷举 5×5 组合：只有 pending→running 与
// running→passed/failed/error 合法，同状态、终态与其余组合全部拒绝。
func TestValidateRetestTransitionMatrix(t *testing.T) {
	allowed := map[RetestStatus]map[RetestStatus]bool{
		RetestPending: {
			RetestRunning: true,
		},
		RetestRunning: {
			RetestPassed: true,
			RetestFailed: true,
			RetestError:  true,
		},
		RetestPassed: {},
		RetestFailed: {},
		RetestError:  {},
	}
	all := []RetestStatus{RetestPending, RetestRunning, RetestPassed, RetestFailed, RetestError}
	for _, from := range all {
		for _, to := range all {
			err := ValidateRetestTransition(from, to)
			if allowed[from][to] {
				if err != nil {
					t.Errorf("%s→%s 应合法，实际：%v", from, to, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s→%s 应非法", from, to)
				continue
			}
			switch {
			case from == to:
				if !errors.Is(err, ErrIllegalRetestTransition) {
					t.Errorf("%s→%s（同状态）应返回 ErrIllegalRetestTransition，实际：%v", from, to, err)
				}
			case from.Terminal():
				if !errors.Is(err, ErrRetestTerminal) {
					t.Errorf("%s→%s（终态）应返回 ErrRetestTerminal，实际：%v", from, to, err)
				}
			default:
				if !errors.Is(err, ErrIllegalRetestTransition) {
					t.Errorf("%s→%s 应返回 ErrIllegalRetestTransition，实际：%v", from, to, err)
				}
			}
		}
	}
}

// TestValidateRetestTransitionUnknownStatuses 校验 from/to 未知时的错误分类。
func TestValidateRetestTransitionUnknownStatuses(t *testing.T) {
	if err := ValidateRetestTransition("unknown", RetestRunning); !errors.Is(err, ErrInvalidRetestStatus) {
		t.Errorf("未知 from 应返回 ErrInvalidRetestStatus，实际：%v", err)
	}
	if err := ValidateRetestTransition(RetestPending, "unknown"); !errors.Is(err, ErrInvalidRetestStatus) {
		t.Errorf("未知 to 应返回 ErrInvalidRetestStatus，实际：%v", err)
	}
}

// TestRetestStatusStrings 锁定复测状态的中文名称。
func TestRetestStatusStrings(t *testing.T) {
	cases := map[RetestStatus]string{
		RetestPending: "待开始",
		RetestRunning: "执行中",
		RetestPassed:  "已通过",
		RetestFailed:  "未通过",
		RetestError:   "执行错误",
	}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("%s.String() = %q，期望 %q", status, got, want)
		}
	}
}
