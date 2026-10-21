package policy

import (
	"fmt"
	"regexp"
	"strings"
)

// Rule 是一条工具调用策略规则。
//
// 匹配顺序由 Priority 决定（越大越先匹配），同优先级保持规则列表中的先后顺序。
// 一条规则同时约束"工具名"与可选的"参数正则"，两者都命中才算命中。
type Rule struct {
	// Name 是规则名，必填且在规则集合内唯一，用于审计与审批展示。
	Name string `json:"name"`
	// Tool 是要匹配的工具名（例如 "shell_exec"、"http_probe"）。
	// 用 "*" 表示匹配所有工具；空字符串容错地按 "*" 处理，但 Validate 会拒绝。
	Tool string `json:"tool"`
	// ArgPattern 是可选的参数正则：非空时对 args 的**原始 JSON 文本**做
	// regexp.MatchString。因为匹配的是序列化文本，正则对参数顺序、空白与转义
	// 敏感（例如引号在 JSON 原文里是 \"）；需要结构化匹配时应在上层解析参数，
	// 而不是把复杂逻辑塞进这条正则。为空表示不限制参数。
	ArgPattern string `json:"arg_pattern"`
	// Action 是命中后的裁决：allow / ask / deny，必填且必须合法。
	Action Action `json:"action"`
	// Priority 决定匹配顺序：越大越先匹配；同优先级保持输入顺序。
	Priority int `json:"priority"`
	// TimeoutSeconds 是 ask 的等待审批超时（秒）。<=0 表示使用上层默认值。
	TimeoutSeconds int `json:"timeout_seconds"`
	// Enabled 是配置层的启用开关：false 表示规则停用。
	// Evaluate 只做纯匹配、不读这个字段；调用方应先用 ActiveRules 过滤（或由
	// 存储层只查询启用规则），停用规则便不会进入匹配。
	Enabled bool `json:"enabled"`
	// Immutable 标记内置底线规则：存储层 / UI 不应删除或修改其动作。
	// 领域层只携带这个标记，不强制；是否强制由持久化与 API 层决定。
	Immutable bool `json:"immutable"`
}

// Validate 校验单条规则的结构合法性：
//
//   - Name 非空（只含空白视为空）；
//   - Tool 非空（只含空白视为空）；"*" 合法；
//   - Action 必须是 allow / ask / deny；
//   - ArgPattern 为空或能编译成合法正则；
//   - TimeoutSeconds 不能为负数（0 表示使用默认超时）；
//   - Priority 任意整数，不在此校验。
//
// Enabled / Immutable 为布尔标记，不影响合法性。
func (r Rule) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("规则名不能为空")
	}
	if strings.TrimSpace(r.Tool) == "" {
		return fmt.Errorf("规则 %q 的 tool 不能为空", r.Name)
	}
	if !r.Action.Valid() {
		return fmt.Errorf("规则 %q 的 action %q 非法（允许 %s / %s / %s）",
			r.Name, string(r.Action), Allow, Ask, Deny)
	}
	if r.TimeoutSeconds < 0 {
		return fmt.Errorf("规则 %q 的 timeout_seconds 不能为负数：%d", r.Name, r.TimeoutSeconds)
	}
	if r.ArgPattern != "" {
		if _, err := regexp.Compile(r.ArgPattern); err != nil {
			return fmt.Errorf("规则 %q 的 arg_pattern 不是合法正则：%w", r.Name, err)
		}
	}
	return nil
}

// ValidateRules 校验一整个规则集合：逐条 Validate，并拒绝重名（名称精确匹配，
// 区分大小写）。集合为空也是合法的——此时 Evaluate 会直接使用默认动作。
//
// 错误信息带规则序号，方便在配置页/日志里定位是第几条规则写错了。
func ValidateRules(rules []Rule) error {
	names := make(map[string]struct{}, len(rules))
	for i, r := range rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("第 %d 条规则：%w", i+1, err)
		}
		if _, dup := names[r.Name]; dup {
			return fmt.Errorf("第 %d 条规则的名称 %q 与前面的规则重复", i+1, r.Name)
		}
		names[r.Name] = struct{}{}
	}
	return nil
}
