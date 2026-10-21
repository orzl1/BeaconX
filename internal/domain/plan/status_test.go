package plan

import "testing"

// TestStepStatusValid 覆盖全部合法状态与若干非法取值。
func TestStepStatusValid(t *testing.T) {
	valid := []StepStatus{
		StepPending, StepReady, StepRunning, StepDone, StepSkipped, StepFailed,
	}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("状态 %q 应合法", string(s))
		}
	}

	invalid := []StepStatus{
		"", "Pending", "PENDING", "done ", " done", "finished", "unknown", "初始",
	}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("状态 %q 应非法", string(s))
		}
	}
}

// TestStepStatusTerminal 覆盖全部状态的终态判定：只有 done / skipped / failed
// 是终态，空白与未知状态保守地不算终态。
func TestStepStatusTerminal(t *testing.T) {
	cases := []struct {
		status StepStatus
		want   bool
	}{
		{StepPending, false},
		{StepReady, false},
		{StepRunning, false},
		{StepDone, true},
		{StepSkipped, true},
		{StepFailed, true},
		{StepStatus(""), false},
		{StepStatus("unknown"), false},
	}
	for _, c := range cases {
		if got := c.status.Terminal(); got != c.want {
			t.Errorf("状态 %q 的 Terminal() = %v，期望 %v", string(c.status), got, c.want)
		}
	}
}

// TestStepStatusString 校验中文名称与未知状态的原文回退。
func TestStepStatusString(t *testing.T) {
	names := map[StepStatus]string{
		StepPending: "待执行",
		StepReady:   "可执行",
		StepRunning: "执行中",
		StepDone:    "已完成",
		StepSkipped: "已跳过",
		StepFailed:  "已失败",
	}
	for status, want := range names {
		if got := status.String(); got != want {
			t.Errorf("状态 %q 的 String() = %q，期望 %q", string(status), got, want)
		}
	}
	for _, unknown := range []StepStatus{"", "unknown", "done "} {
		if got := unknown.String(); got != string(unknown) {
			t.Errorf("未知状态 %q 的 String() 应回退为原值，实际 %q", string(unknown), got)
		}
	}
}
