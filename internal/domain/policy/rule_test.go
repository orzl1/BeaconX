package policy

import (
	"strings"
	"testing"
)

// validRule 返回一条可以通过校验的规则，便于各用例只改一个字段。
func validRule() Rule {
	return Rule{
		Name:           "测试规则",
		Tool:           "shell_exec",
		ArgPattern:     `rm\s+-rf`,
		Action:         Deny,
		Priority:       10,
		TimeoutSeconds: 0,
		Enabled:        true,
	}
}

// TestRuleValidateOK 覆盖所有应当通过的规则形态。
func TestRuleValidateOK(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
	}{
		{name: "典型拒绝规则", rule: validRule()},
		{name: "通配工具", rule: func() Rule { r := validRule(); r.Tool = "*"; return r }()},
		{name: "空参数正则", rule: func() Rule { r := validRule(); r.ArgPattern = ""; return r }()},
		{name: "允许规则", rule: func() Rule { r := validRule(); r.Action = Allow; return r }()},
		{name: "询问规则", rule: func() Rule { r := validRule(); r.Action = Ask; r.TimeoutSeconds = 60; return r }()},
		{name: "负优先级合法", rule: func() Rule { r := validRule(); r.Priority = -100; return r }()},
		{name: "零优先级合法", rule: func() Rule { r := validRule(); r.Priority = 0; return r }()},
		{name: "停用规则合法", rule: func() Rule { r := validRule(); r.Enabled = false; return r }()},
		{name: "不可变规则合法", rule: func() Rule { r := validRule(); r.Immutable = true; return r }()},
		{name: "带转义引号的正则", rule: func() Rule { r := validRule(); r.ArgPattern = `\"method\"`; return r }()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.rule.Validate(); err != nil {
				t.Fatalf("应通过校验，实际错误：%v", err)
			}
		})
	}
}

// TestRuleValidateErrors 覆盖各类非法规则，用错误子串断言。
func TestRuleValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want string
	}{
		{name: "空规则名", rule: func() Rule { r := validRule(); r.Name = ""; return r }(), want: "规则名不能为空"},
		{name: "空白规则名", rule: func() Rule { r := validRule(); r.Name = "  \t"; return r }(), want: "规则名不能为空"},
		{name: "空工具名", rule: func() Rule { r := validRule(); r.Tool = ""; return r }(), want: "tool 不能为空"},
		{name: "空白工具名", rule: func() Rule { r := validRule(); r.Tool = "   "; return r }(), want: "tool 不能为空"},
		{name: "空动作", rule: func() Rule { r := validRule(); r.Action = ""; return r }(), want: "action"},
		{name: "非法动作", rule: func() Rule { r := validRule(); r.Action = "block"; return r }(), want: `action "block" 非法`},
		{name: "动作大小写敏感", rule: func() Rule { r := validRule(); r.Action = "Allow"; return r }(), want: "action"},
		{name: "非法正则", rule: func() Rule { r := validRule(); r.ArgPattern = "["; return r }(), want: "arg_pattern 不是合法正则"},
		{name: "未闭合分组", rule: func() Rule { r := validRule(); r.ArgPattern = "(abc"; return r }(), want: "arg_pattern 不是合法正则"},
		{name: "负超时", rule: func() Rule { r := validRule(); r.TimeoutSeconds = -1; return r }(), want: "timeout_seconds 不能为负数"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.rule.Validate()
			if err == nil {
				t.Fatal("应校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应包含 %q，实际为 %q", c.want, err.Error())
			}
		})
	}
}

// TestValidateRules 覆盖整份规则集的校验：空集合、合法集合、重名、定位到第几条。
func TestValidateRules(t *testing.T) {
	t.Run("空集合合法", func(t *testing.T) {
		if err := ValidateRules(nil); err != nil {
			t.Fatalf("空规则集应合法，实际错误：%v", err)
		}
	})

	t.Run("默认规则集合法", func(t *testing.T) {
		if err := ValidateRules(SeedDefaults()); err != nil {
			t.Fatalf("默认规则集应全部合法，实际错误：%v", err)
		}
	})

	t.Run("不同名的合法规则集", func(t *testing.T) {
		r1 := validRule()
		r2 := validRule()
		r2.Name = "另一条规则"
		if err := ValidateRules([]Rule{r1, r2}); err != nil {
			t.Fatalf("应合法，实际错误：%v", err)
		}
	})

	t.Run("重名检测", func(t *testing.T) {
		r1 := validRule()
		r2 := validRule() // 同名
		r2.Priority = 99
		err := ValidateRules([]Rule{r1, r2})
		if err == nil || !strings.Contains(err.Error(), "重复") {
			t.Fatalf("应检测到重名，实际错误：%v", err)
		}
	})

	t.Run("名称精确匹配区分大小写", func(t *testing.T) {
		r1 := validRule()
		r2 := validRule()
		r2.Name = "TEST规则" // 与「测试规则」不同
		if err := ValidateRules([]Rule{r1, r2}); err != nil {
			t.Fatalf("名称不同应合法，实际错误：%v", err)
		}
	})

	t.Run("非法规则定位到序号", func(t *testing.T) {
		r1 := validRule()
		r2 := validRule()
		r2.Name = "坏规则"
		r2.ArgPattern = "["
		err := ValidateRules([]Rule{r1, r2})
		if err == nil {
			t.Fatal("应返回错误")
		}
		if !strings.Contains(err.Error(), "第 2 条规则") {
			t.Fatalf("错误应定位到第 2 条规则，实际为 %q", err.Error())
		}
	})
}
