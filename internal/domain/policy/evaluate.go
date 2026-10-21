package policy

import (
	"fmt"
	"regexp"
	"sort"
)

// Decision 是一次策略评估的结果。
type Decision struct {
	// Action 是最终裁决：allow / ask / deny。
	Action Action
	// RuleName 是命中的规则名；使用默认动作时为空字符串。
	RuleName string
	// Reason 是面向人的中文说明（命中哪条规则 / 为什么用默认动作）。
	Reason string
}

// wildcardTool 是"匹配所有工具"的规则工具名。
const wildcardTool = "*"

// Evaluate 按策略规则评估一次工具调用。
//
// 匹配规则：
//
//  1. 按 Priority 降序排序（sort.SliceStable，同优先级保持 rules 的输入顺序）；
//     输入切片不会被修改，排序发生在副本上；
//  2. 工具匹配：规则 Tool 为 "*"（或空字符串，容错）匹配所有工具；
//     否则要求与 tool 参数精确相等；
//  3. 若规则 ArgPattern 非空，用 regexp.MatchString 匹配 argsJSON 的**原文**
//     （不是解析后的字段）；
//  4. 第一条同时满足 2/3 的规则决定结果，返回其 Action 与 Name；
//  5. 没有规则命中时使用 defaultAction，Decision.RuleName 为空。
//
// Evaluate 只做纯匹配，**不按 Enabled 过滤**：Enabled 属于持久化/UI 层的开关，
// 调用方应先只传出生效规则（可用 ActiveRules，或由存储层只查询 enabled 规则）。
// 这样领域函数只有一条清晰的匹配路径，不会出现"没传对参数导致规则被静默跳过"。
//
// 错误（fail closed）：
//
//   - defaultAction 不是 allow / ask / deny；
//   - 任一传入规则的正则无法编译（即使它在排序上更低、本不会命中）；
//   - 命中的规则 Action 非法（调用方未经 ValidateRules 直接使用脏数据时）。
//
// 推荐调用方在"无规则命中"时传 Ask，而不是 Allow：策略缺失应该带来人工确认，
// 而不是静默放行。
func Evaluate(tool string, argsJSON []byte, rules []Rule, defaultAction Action) (Decision, error) {
	if !defaultAction.Valid() {
		return Decision{}, fmt.Errorf("默认动作 %q 非法（允许 allow / ask / deny）", string(defaultAction))
	}

	// 排序发生在副本上，保证 Evaluate 不修改调用方传入的切片。
	sorted := make([]Rule, len(rules))
	copy(sorted, rules)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority > sorted[j].Priority
	})

	// 预编译所有规则的正则：配置错误一律显式报错，而不是静默跳过一条
	// 本该生效的规则（安全组件不能把"配置写错"降级成"没这条规则"）。
	compiled := make([]*regexp.Regexp, len(sorted))
	for i, r := range sorted {
		if r.ArgPattern == "" {
			continue
		}
		re, err := regexp.Compile(r.ArgPattern)
		if err != nil {
			return Decision{}, fmt.Errorf("规则 %q 的 arg_pattern 不是合法正则：%w", r.Name, err)
		}
		compiled[i] = re
	}

	args := string(argsJSON)
	for i := range sorted {
		r := sorted[i]
		if !toolMatches(r.Tool, tool) {
			continue
		}
		if re := compiled[i]; re != nil && !re.MatchString(args) {
			continue
		}
		if !r.Action.Valid() {
			return Decision{}, fmt.Errorf("命中的规则 %q 的 action %q 非法（允许 allow / ask / deny）",
				r.Name, string(r.Action))
		}
		return Decision{
			Action:   r.Action,
			RuleName: r.Name,
			Reason:   fmt.Sprintf("命中规则「%s」：%s", r.Name, r.Action.String()),
		}, nil
	}

	return Decision{
		Action: defaultAction,
		Reason: fmt.Sprintf("未命中任何规则，使用默认动作：%s", defaultAction.String()),
	}, nil
}

// ActiveRules 返回 rules 中 Enabled=true 的规则副本（保持输入顺序）。
//
// Evaluate 本身不做 Enabled 过滤，调用方在评估前应先用本函数过滤，或让存储层
// 只查询启用规则。之所以把"过滤"与"匹配"分开：Enabled 是配置生命周期问题，
// 匹配是策略语义问题，混在一起后很难解释"为什么这条规则没生效"。
func ActiveRules(rules []Rule) []Rule {
	var active []Rule
	for _, r := range rules {
		if r.Enabled {
			active = append(active, r)
		}
	}
	return active
}

// toolMatches 报告规则的 tool 约束是否匹配本次工具调用。
// "*" 与空字符串（容错）匹配所有工具；其余精确匹配。
func toolMatches(ruleTool, tool string) bool {
	if ruleTool == "" || ruleTool == wildcardTool {
		return true
	}
	return ruleTool == tool
}
