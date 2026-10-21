package plan

import (
	"fmt"
	"strings"
	"testing"
)

// stepsOf 生成 n 个互不依赖、key 合法的步骤，用于规模上限用例。
func stepsOf(n int) []Step {
	steps := make([]Step, 0, n)
	for i := 0; i < n; i++ {
		steps = append(steps, Step{Key: fmt.Sprintf("s%d", i), Title: fmt.Sprintf("步骤 %d", i)})
	}
	return steps
}

// actionsOf 生成 n 个引用首个步骤的动作，用于规模上限用例。
func actionsOf(n int) []Action {
	actions := make([]Action, 0, n)
	for i := 0; i < n; i++ {
		actions = append(actions, Action{StepKey: "s0", Kind: ActionKindProbe, Target: fmt.Sprintf("t%d", i)})
	}
	return actions
}

// TestDecisionValidateOK 覆盖所有应当通过的形态，包括上限边界。
func TestDecisionValidateOK(t *testing.T) {
	cases := []struct {
		name string
		d    Decision
	}{
		{name: "空决策", d: Decision{}},
		{name: "仅结束", d: Decision{Done: true, Reason: "目标已达成"}},
		{name: "单步骤", d: Decision{Steps: []Step{{Key: "recon", Title: "侦察"}}}},
		{name: "依赖链", d: Decision{Steps: []Step{
			{Key: "recon", Title: "侦察"},
			{Key: "enum", Title: "枚举", DependsOn: []string{"recon"}},
			{Key: "verify", Title: "验证", DependsOn: []string{"enum"}},
		}}},
		{name: "菱形依赖", d: Decision{Steps: []Step{
			{Key: "a"},
			{Key: "b", DependsOn: []string{"a"}},
			{Key: "c", DependsOn: []string{"a"}},
			{Key: "d", DependsOn: []string{"b", "c"}},
		}}},
		{name: "依赖后声明的步骤", d: Decision{Steps: []Step{
			{Key: "b", DependsOn: []string{"a"}},
			{Key: "a"},
		}}},
		{name: "动作引用本步骤", d: Decision{
			Steps:   []Step{{Key: "recon"}},
			Actions: []Action{{StepKey: "recon", Kind: ActionKindProbe, Target: "example.com"}},
		}},
		{name: "四种合法动作", d: Decision{
			Steps: []Step{{Key: "s"}},
			Actions: []Action{
				{StepKey: "s", Kind: ActionKindProbe},
				{StepKey: "s", Kind: ActionKindVerify},
				{StepKey: "s", Kind: ActionKindReport},
				{StepKey: "s", Kind: ActionKindOther},
			},
		}},
		{name: "步骤数上限边界", d: Decision{Steps: stepsOf(MaxSteps)}},
		{name: "动作数上限边界", d: Decision{Steps: stepsOf(1), Actions: actionsOf(MaxActions)}},
		{name: "key 允许数字与短横线", d: Decision{Steps: []Step{
			{Key: "recon-2"}, {Key: "9_scan", DependsOn: []string{"recon-2"}},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.d.Validate(); err != nil {
				t.Fatalf("应通过校验，实际错误：%v", err)
			}
		})
	}
}

// TestDecisionValidateErrors 覆盖各类非法决策；用错误子串断言，避免把错误
// 文案完全锁死。
func TestDecisionValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		d    Decision
		want string
	}{
		{
			name: "步骤 key 为空",
			d:    Decision{Steps: []Step{{Key: ""}}},
			want: "key 不能为空",
		},
		{
			name: "步骤 key 含大写字母",
			d:    Decision{Steps: []Step{{Key: "Recon"}}},
			want: "非法字符",
		},
		{
			name: "步骤 key 含点号",
			d:    Decision{Steps: []Step{{Key: "web.enum"}}},
			want: "非法字符",
		},
		{
			name: "步骤 key 含空格",
			d:    Decision{Steps: []Step{{Key: "web enum"}}},
			want: "非法字符",
		},
		{
			name: "步骤 key 超长",
			d:    Decision{Steps: []Step{{Key: strings.Repeat("a", MaxStepKeyLen+1)}}},
			want: "超过上限 64",
		},
		{
			name: "重复步骤 key",
			d:    Decision{Steps: []Step{{Key: "recon"}, {Key: "recon"}}},
			want: `重复`,
		},
		{
			name: "步骤数超限",
			d:    Decision{Steps: stepsOf(MaxSteps + 1)},
			want: "步骤数量 21 超过上限 20",
		},
		{
			name: "依赖不存在的步骤",
			d:    Decision{Steps: []Step{{Key: "b", DependsOn: []string{"a"}}}},
			want: `依赖的步骤 "a" 不存在`,
		},
		{
			name: "依赖 key 为空",
			d:    Decision{Steps: []Step{{Key: "a", DependsOn: []string{""}}}},
			want: "依赖的 key 不能为空",
		},
		{
			name: "自依赖",
			d:    Decision{Steps: []Step{{Key: "a", DependsOn: []string{"a"}}}},
			want: "不能依赖自身",
		},
		{
			name: "重复依赖",
			d:    Decision{Steps: []Step{{Key: "a"}, {Key: "b", DependsOn: []string{"a", "a"}}}},
			want: "重复依赖",
		},
		{
			name: "两节点依赖环",
			d: Decision{Steps: []Step{
				{Key: "a", DependsOn: []string{"b"}},
				{Key: "b", DependsOn: []string{"a"}},
			}},
			want: "检测到依赖环：a -> b -> a",
		},
		{
			name: "三节点依赖环",
			d: Decision{Steps: []Step{
				{Key: "a", DependsOn: []string{"c"}},
				{Key: "b", DependsOn: []string{"a"}},
				{Key: "c", DependsOn: []string{"b"}},
			}},
			want: "检测到依赖环",
		},
		{
			name: "动作数超限",
			d:    Decision{Steps: stepsOf(1), Actions: actionsOf(MaxActions + 1)},
			want: "动作数量 11 超过上限 10",
		},
		{
			name: "动作缺少 step_key",
			d:    Decision{Steps: stepsOf(1), Actions: []Action{{Kind: ActionKindProbe}}},
			want: "缺少 step_key",
		},
		{
			name: "动作引用不存在的步骤",
			d:    Decision{Steps: stepsOf(1), Actions: []Action{{StepKey: "missing", Kind: ActionKindProbe}}},
			want: `引用的步骤 "missing" 不存在`,
		},
		{
			name: "动作 kind 为空",
			d:    Decision{Steps: stepsOf(1), Actions: []Action{{StepKey: "s0"}}},
			want: "kind",
		},
		{
			name: "动作 kind 非法",
			d:    Decision{Steps: stepsOf(1), Actions: []Action{{StepKey: "s0", Kind: "exploit"}}},
			want: `kind "exploit" 非法`,
		},
		{
			name: "动作 kind 大小写敏感",
			d:    Decision{Steps: stepsOf(1), Actions: []Action{{StepKey: "s0", Kind: "Probe"}}},
			want: "kind",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.d.Validate()
			if err == nil {
				t.Fatalf("应校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应包含 %q，实际为 %q", c.want, err.Error())
			}
		})
	}
}

// TestValidateWithKnown 校验「本次步骤 ∪ 已知步骤」的引用语义。
func TestValidateWithKnown(t *testing.T) {
	known := map[string]bool{"recon": true, "scan": true}

	t.Run("依赖已知步骤", func(t *testing.T) {
		d := Decision{Steps: []Step{{Key: "verify", DependsOn: []string{"recon"}}}}
		if err := d.Validate(); err == nil {
			t.Fatal("Validate 不应把已知步骤当成本次步骤")
		}
		if err := d.ValidateWithKnown(known); err != nil {
			t.Fatalf("ValidateWithKnown 应允许依赖已知步骤，实际错误：%v", err)
		}
	})

	t.Run("动作引用已知步骤", func(t *testing.T) {
		d := Decision{Actions: []Action{{StepKey: "scan", Kind: ActionKindProbe}}}
		if err := d.Validate(); err == nil {
			t.Fatal("Validate 不应允许动作引用未知步骤")
		}
		if err := d.ValidateWithKnown(known); err != nil {
			t.Fatalf("ValidateWithKnown 应允许动作引用已知步骤，实际错误：%v", err)
		}
	})

	t.Run("已知集合为空时的退化行为", func(t *testing.T) {
		d := Decision{Steps: []Step{{Key: "verify", DependsOn: []string{"recon"}}}}
		if err := d.ValidateWithKnown(nil); err == nil {
			t.Fatal("nil 已知集合应等价于 Validate")
		}
	})

	t.Run("既不在本次也不在已知集合", func(t *testing.T) {
		d := Decision{Steps: []Step{{Key: "verify", DependsOn: []string{"other"}}}}
		if err := d.ValidateWithKnown(known); err == nil {
			t.Fatal("未知步骤仍应报错")
		}
	})

	t.Run("环检测不受已知集合影响", func(t *testing.T) {
		d := Decision{Steps: []Step{
			{Key: "a", DependsOn: []string{"recon", "b"}},
			{Key: "b", DependsOn: []string{"a"}},
		}}
		err := d.ValidateWithKnown(known)
		if err == nil || !strings.Contains(err.Error(), "依赖环") {
			t.Fatalf("应检测到本次步骤之间的环，实际错误：%v", err)
		}
	})
}
