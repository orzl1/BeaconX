package plan

import (
	"slices"
	"testing"
)

// TestReadySteps 覆盖依赖满足/未满足、自身状态、终态与跨决策依赖等情形。
func TestReadySteps(t *testing.T) {
	steps := []Step{
		{Key: "recon", Title: "侦察"},
		{Key: "enum", Title: "枚举", DependsOn: []string{"recon"}},
		{Key: "verify", Title: "验证", DependsOn: []string{"enum"}},
	}

	cases := []struct {
		name   string
		steps  []Step
		status map[string]StepStatus
		want   []string
	}{
		{
			name:   "无依赖全部待执行",
			steps:  steps,
			status: map[string]StepStatus{"recon": StepPending, "enum": StepPending, "verify": StepPending},
			want:   []string{"recon"},
		},
		{
			name:   "依赖已完成后就绪",
			steps:  steps,
			status: map[string]StepStatus{"recon": StepDone, "enum": StepPending, "verify": StepPending},
			want:   []string{"enum"},
		},
		{
			name:   "依赖未完成不就绪",
			steps:  steps,
			status: map[string]StepStatus{"recon": StepRunning, "enum": StepPending},
			want:   nil,
		},
		{
			name:   "依赖为终态但非 done 不就绪",
			steps:  steps,
			status: map[string]StepStatus{"recon": StepFailed, "enum": StepPending},
			want:   nil,
		},
		{
			name:   "依赖被跳过不就绪",
			steps:  steps,
			status: map[string]StepStatus{"recon": StepSkipped, "enum": StepPending},
			want:   nil,
		},
		{
			name:   "自身运行中且依赖未完成都不返回",
			steps:  steps,
			status: map[string]StepStatus{"recon": StepRunning, "enum": StepPending},
			want:   nil,
		},
		{
			name:   "缺失状态的步骤不参与派发",
			steps:  steps,
			status: map[string]StepStatus{"recon": StepPending},
			want:   []string{"recon"},
		},
		{
			name:   "status 为空时无步骤就绪",
			steps:  steps,
			status: nil,
			want:   nil,
		},
		{
			name:   "空步骤列表",
			steps:  nil,
			status: map[string]StepStatus{"recon": StepPending},
			want:   nil,
		},
		{
			name: "跨决策依赖：已知步骤已 done 则可就绪",
			steps: []Step{
				{Key: "verify", DependsOn: []string{"prior_recon"}},
			},
			status: map[string]StepStatus{"verify": StepPending, "prior_recon": StepDone},
			want:   []string{"verify"},
		},
		{
			name: "跨决策依赖：已知步骤未完成则不就绪",
			steps: []Step{
				{Key: "verify", DependsOn: []string{"prior_recon"}},
			},
			status: map[string]StepStatus{"verify": StepPending, "prior_recon": StepRunning},
			want:   nil,
		},
		{
			name: "按输入顺序返回",
			steps: []Step{
				{Key: "b"},
				{Key: "a"},
				{Key: "c", DependsOn: []string{"a"}},
			},
			status: map[string]StepStatus{"a": StepPending, "b": StepPending, "c": StepPending},
			want:   []string{"b", "a"},
		},
		{
			name: "多依赖需全部 done",
			steps: []Step{
				{Key: "join", DependsOn: []string{"a", "b"}},
			},
			status: map[string]StepStatus{"join": StepPending, "a": StepDone, "b": StepDone},
			want:   []string{"join"},
		},
		{
			name: "多依赖有一个未完成",
			steps: []Step{
				{Key: "join", DependsOn: []string{"a", "b"}},
			},
			status: map[string]StepStatus{"join": StepPending, "a": StepDone, "b": StepPending},
			want:   nil,
		},
		{
			name: "终态自身不返回",
			steps: []Step{
				{Key: "a"},
				{Key: "b"},
				{Key: "c"},
			},
			status: map[string]StepStatus{"a": StepDone, "b": StepSkipped, "c": StepFailed},
			want:   nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReadySteps(c.steps, c.status)
			if !slices.Equal(got, c.want) {
				t.Fatalf("ReadySteps() = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestReadyStepsReturnsNilWhenEmpty 明确空结果的形态：返回 nil 切片，调用方
// 用 len()/slices.Equal 判断即可。
func TestReadyStepsReturnsNilWhenEmpty(t *testing.T) {
	got := ReadySteps([]Step{{Key: "a"}}, map[string]StepStatus{"a": StepRunning})
	if got != nil {
		t.Fatalf("无就绪步骤时应返回 nil，实际 %#v", got)
	}
}
