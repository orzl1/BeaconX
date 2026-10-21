package plan

import (
	"fmt"
	"strings"
)

// Validate 校验决策自身的结构合法性，不依赖任何外部状态。
//
// 校验顺序与规则：
//
//  1. 规模：步骤数 ≤ MaxSteps、动作数 ≤ MaxActions；
//  2. 步骤 key：非空、格式合法（小写字母 / 数字 / '_' / '-'，长度 1..64）、
//     同一决策内唯一；
//  3. 依赖：key 非空、不能自依赖、不能重复、必须能在本次 Steps 中找到
//     （ValidateWithKnown 额外允许已知步骤集合）；
//  4. 环检测：步骤之间不允许出现依赖环；
//  5. 动作：StepKey 非空且指向本次或已知步骤，Kind 必须是合法动作类型。
//
// 依赖与动作的引用都要求「本次步骤 ∪ 已知步骤」——规划是增量的，后一次决策
// 的步骤可以依赖前一次决策已经声明的步骤。
func (d Decision) Validate() error {
	return d.ValidateWithKnown(nil)
}

// ValidateWithKnown 与 Validate 相同，但允许步骤依赖 / 动作引用已知步骤集合
// 中的 key。known 为 nil 时等价于 Validate。
//
// known 只提供「该 key 存在」这一事实，不提供依赖信息，因此 known 里的步骤
// 不参与环检测：环只可能出现在本次决策声明的步骤之间。
func (d Decision) ValidateWithKnown(known map[string]bool) error {
	if len(d.Steps) > MaxSteps {
		return fmt.Errorf("步骤数量 %d 超过上限 %d", len(d.Steps), MaxSteps)
	}
	if len(d.Actions) > MaxActions {
		return fmt.Errorf("动作数量 %d 超过上限 %d", len(d.Actions), MaxActions)
	}

	// 第一步：key 合法性与唯一性。先建好集合，后面依赖/动作引用才能不区分
	// 声明顺序（允许依赖后声明的步骤）。
	stepKeys := make(map[string]bool, len(d.Steps))
	for i, s := range d.Steps {
		if err := validateStepKey(s.Key); err != nil {
			return fmt.Errorf("第 %d 个步骤：%w", i+1, err)
		}
		if stepKeys[s.Key] {
			return fmt.Errorf("步骤 key %q 重复", s.Key)
		}
		stepKeys[s.Key] = true
	}

	// 第二步：依赖关系。重复依赖按"数据未规范化"直接拒绝；ParseDecision 会在
	// 校验前替模型输出去重（见 parse.go）。
	for _, s := range d.Steps {
		seenDeps := make(map[string]bool, len(s.DependsOn))
		for _, dep := range s.DependsOn {
			if dep == "" {
				return fmt.Errorf("步骤 %q 依赖的 key 不能为空", s.Key)
			}
			if dep == s.Key {
				return fmt.Errorf("步骤 %q 不能依赖自身", s.Key)
			}
			if seenDeps[dep] {
				return fmt.Errorf("步骤 %q 重复依赖 %q", s.Key, dep)
			}
			seenDeps[dep] = true
			if !stepKeys[dep] && !known[dep] {
				return fmt.Errorf("步骤 %q 依赖的步骤 %q 不存在", s.Key, dep)
			}
		}
	}

	// 第三步：环检测（只在本次声明的步骤之间）。
	if cycle := findDependencyCycle(d.Steps, stepKeys); cycle != nil {
		return fmt.Errorf("检测到依赖环：%s", strings.Join(cycle, " -> "))
	}

	// 第四步：动作。
	for i, a := range d.Actions {
		if a.StepKey == "" {
			return fmt.Errorf("第 %d 个动作缺少 step_key", i+1)
		}
		if !stepKeys[a.StepKey] && !known[a.StepKey] {
			return fmt.Errorf("动作引用的步骤 %q 不存在", a.StepKey)
		}
		if !validActionKind(a.Kind) {
			return fmt.Errorf("动作的 kind %q 非法（允许 %s / %s / %s / %s）",
				a.Kind, ActionKindProbe, ActionKindVerify, ActionKindReport, ActionKindOther)
		}
	}
	return nil
}

// validateStepKey 校验步骤 key：非空、长度 1..64、只含小写字母 / 数字 / '_' / '-'。
func validateStepKey(key string) error {
	if key == "" {
		return fmt.Errorf("步骤 key 不能为空")
	}
	if len(key) > MaxStepKeyLen {
		return fmt.Errorf("步骤 key %q 长度 %d 超过上限 %d", key, len(key), MaxStepKeyLen)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("步骤 key %q 含非法字符（第 %d 个字节，只允许小写字母、数字、'_'、'-'）", key, i+1)
		}
	}
	return nil
}

// findDependencyCycle 在本次决策声明的步骤之间寻找依赖环，返回环上的 key 序列
// （首尾相同，例如 ["a", "b", "a"]）；无环返回 nil。
//
// 只跟随指向本次步骤的依赖边；依赖 known 集合的边没有依赖信息，不参与检测。
// 遍历按 Steps 的声明顺序、DependsOn 的书写顺序进行，因此同一个输入的报错
// 是稳定的（便于测试与排障）。
func findDependencyCycle(steps []Step, stepKeys map[string]bool) []string {
	edges := make(map[string][]string, len(steps))
	for _, s := range steps {
		for _, dep := range s.DependsOn {
			if stepKeys[dep] {
				edges[s.Key] = append(edges[s.Key], dep)
			}
		}
	}

	const (
		white = 0 // 未访问
		gray  = 1 // 在当前 DFS 栈上
		black = 2 // 已完成
	)
	color := make(map[string]int, len(steps))
	var stack []string

	var dfs func(key string) []string
	dfs = func(key string) []string {
		color[key] = gray
		stack = append(stack, key)
		for _, dep := range edges[key] {
			switch color[dep] {
			case gray:
				// 找到回边：从栈中 dep 首次出现的位置截取环。
				start := 0
				for i, k := range stack {
					if k == dep {
						start = i
						break
					}
				}
				cycle := append([]string(nil), stack[start:]...)
				return append(cycle, dep)
			case white:
				if cycle := dfs(dep); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[key] = black
		return nil
	}

	for _, s := range steps {
		if color[s.Key] == white {
			if cycle := dfs(s.Key); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}
