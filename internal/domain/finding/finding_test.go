package finding

import (
	"errors"
	"testing"
)

// TestSeverityValid 覆盖全部合法严重级别与常见非法取值。
func TestSeverityValid(t *testing.T) {
	valid := []Severity{SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("严重级别 %q 应合法", string(s))
		}
		if s.String() == string(s) {
			t.Errorf("严重级别 %q 缺少中文名称", string(s))
		}
	}
	for _, s := range []Severity{"", "unknown", "High", "CRITICAL", "medium "} {
		if s.Valid() {
			t.Errorf("严重级别 %q 应非法", string(s))
		}
	}
}

// TestStatusValidAndTerminal 覆盖状态枚举与终态判定。
func TestStatusValidAndTerminal(t *testing.T) {
	valid := []Status{StatusOpen, StatusConfirmed, StatusFixed, StatusRetest, StatusClosed, StatusFalsePositive}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("状态 %q 应合法", string(s))
		}
		if s.String() == string(s) {
			t.Errorf("状态 %q 缺少中文名称", string(s))
		}
	}
	for _, s := range []Status{"", "done", "Open", "false-positive"} {
		if s.Valid() {
			t.Errorf("状态 %q 应非法", string(s))
		}
		if s.Terminal() {
			t.Errorf("非法状态 %q 不应被判定为终态", string(s))
		}
	}
	for _, s := range []Status{StatusClosed, StatusFalsePositive} {
		if !s.Terminal() {
			t.Errorf("状态 %q 应为终态", string(s))
		}
	}
	for _, s := range []Status{StatusOpen, StatusConfirmed, StatusFixed, StatusRetest} {
		if s.Terminal() {
			t.Errorf("状态 %q 不应为终态", string(s))
		}
	}
}

// TestValidateTransitionMatrix 穷举 6×6 组合：合法迁移返回 nil，非法迁移
// 返回错误且错误类别符合约定（同状态/终态/普通非法各有哨兵）。
func TestValidateTransitionMatrix(t *testing.T) {
	allowed := map[Status]map[Status]bool{
		StatusOpen: {
			StatusConfirmed:     true,
			StatusFalsePositive: true,
			StatusClosed:        true,
		},
		StatusConfirmed: {
			StatusFixed:  true,
			StatusRetest: true,
			StatusClosed: true,
		},
		StatusRetest: {
			StatusConfirmed: true,
			StatusFixed:     true,
			StatusClosed:    true,
		},
		StatusFixed: {
			StatusRetest: true,
			StatusClosed: true,
		},
		StatusClosed:        {},
		StatusFalsePositive: {},
	}
	all := []Status{StatusOpen, StatusConfirmed, StatusFixed, StatusRetest, StatusClosed, StatusFalsePositive}
	for _, from := range all {
		for _, to := range all {
			err := ValidateTransition(from, to)
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
				if !errors.Is(err, ErrIllegalTransition) {
					t.Errorf("%s→%s（同状态）应返回 ErrIllegalTransition，实际：%v", from, to, err)
				}
			case from.Terminal():
				if !errors.Is(err, ErrTerminalStatus) {
					t.Errorf("%s→%s（终态）应返回 ErrTerminalStatus，实际：%v", from, to, err)
				}
			default:
				if !errors.Is(err, ErrIllegalTransition) {
					t.Errorf("%s→%s 应返回 ErrIllegalTransition，实际：%v", from, to, err)
				}
			}
		}
	}
}

// TestValidateTransitionUnknownStatuses 校验 from/to 未知时的错误分类。
func TestValidateTransitionUnknownStatuses(t *testing.T) {
	if err := ValidateTransition("unknown", StatusOpen); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("未知 from 应返回 ErrInvalidStatus，实际：%v", err)
	}
	if err := ValidateTransition(StatusOpen, "unknown"); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("未知 to 应返回 ErrInvalidStatus，实际：%v", err)
	}
}

// TestStatusStrings 锁定中文名称，避免日志与错误信息回退成英文原值。
func TestStatusStrings(t *testing.T) {
	cases := map[Status]string{
		StatusOpen:          "待处理",
		StatusConfirmed:     "已确认",
		StatusFixed:         "已修复",
		StatusRetest:        "待复测",
		StatusClosed:        "已关闭",
		StatusFalsePositive: "误报",
	}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("%s.String() = %q，期望 %q", status, got, want)
		}
	}
}
