package plan

import (
	"strings"
	"testing"
)

// fence 用 ```json 包裹一段内容，避免在测试源码里直接写三反引号。
func fence(body string) string { return "```json\n" + body + "\n```" }

// TestParseDecisionOK 覆盖纯 JSON、围栏、杂质、字符串内花括号等合法输入。
func TestParseDecisionOK(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// wantSteps / wantActions 用于断言解析结果的关键字段。
		wantSteps   int
		wantActions int
		wantDone    bool
		wantSummary string
	}{
		{
			name:      "纯 JSON 对象",
			raw:       `{"summary":"开始侦察","steps":[{"key":"recon","title":"侦察"}]}`,
			wantSteps: 1,
		},
		{
			name:        "空对象",
			raw:         `{}`,
			wantSteps:   0,
			wantSummary: "",
		},
		{
			name:      "json 围栏",
			raw:       fence(`{"steps":[{"key":"recon","title":"侦察"}],"actions":[{"step_key":"recon","kind":"probe"}]}`),
			wantSteps: 1, wantActions: 1,
		},
		{
			name:      "大写 JSON 围栏",
			raw:       "```JSON\n{\"steps\":[{\"key\":\"recon\"}]}\n```",
			wantSteps: 1,
		},
		{
			name:      "无语言标记围栏",
			raw:       "```\n{\"steps\":[{\"key\":\"recon\"}]}\n```",
			wantSteps: 1,
		},
		{
			name:      "单行围栏",
			raw:       "```json {\"steps\":[{\"key\":\"recon\"}]}```",
			wantSteps: 1,
		},
		{
			name:      "无语言标记的单行围栏",
			raw:       "``` {\"steps\":[{\"key\":\"recon\"}]}```",
			wantSteps: 1,
		},
		{
			name:      "字符串内转义引号与花括号",
			raw:       `{"summary":"他说 \"用 {a} 表示\"","steps":[{"key":"recon"}]}`,
			wantSteps: 1,
		},
		{
			name:      "围栏前后夹带说明文字",
			raw:       "好的，下面是本轮规划：\n" + fence(`{"summary":"第一阶段","steps":[{"key":"recon","title":"侦察"}]}`) + "\n以上。",
			wantSteps: 1, wantSummary: "第一阶段",
		},
		{
			name:      "裸 JSON 前有说明",
			raw:       `本轮决策如下：{"steps":[{"key":"recon"}]}`,
			wantSteps: 1,
		},
		{
			name:      "裸 JSON 后有说明",
			raw:       `{"steps":[{"key":"recon"}]} 请执行。`,
			wantSteps: 1,
		},
		{
			name:      "字符串内的花括号不影响配对",
			raw:       `{"summary":"用 {a} 表示","detail":"x","steps":[{"key":"recon","detail":"模板：{{x}}"}]}`,
			wantSteps: 1, wantSummary: "用 {a} 表示",
		},
		{
			name:        "结束决策",
			raw:         `{"done":true,"reason":"目标已达成"}`,
			wantDone:    true,
			wantSummary: "",
		},
		{
			name:        "忽略未知字段",
			raw:         `{"summary":"x","unknown_field":123,"steps":[{"key":"recon","extra":true}]}`,
			wantSummary: "x", wantSteps: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseDecision(c.raw)
			if err != nil {
				t.Fatalf("应解析成功，实际错误：%v", err)
			}
			if len(d.Steps) != c.wantSteps {
				t.Fatalf("步骤数 = %d，期望 %d", len(d.Steps), c.wantSteps)
			}
			if len(d.Actions) != c.wantActions {
				t.Fatalf("动作数 = %d，期望 %d", len(d.Actions), c.wantActions)
			}
			if d.Done != c.wantDone {
				t.Fatalf("Done = %v，期望 %v", d.Done, c.wantDone)
			}
			if c.wantSummary != "" && d.Summary != c.wantSummary {
				t.Fatalf("Summary = %q，期望 %q", d.Summary, c.wantSummary)
			}
		})
	}
}

// TestParseDecisionDedupesDependsOn 校验「解析阶段替模型去重依赖」的语义：
// Validate 本身对重复依赖严格报错，但 ParseDecision 会先规范化再校验。
func TestParseDecisionDedupesDependsOn(t *testing.T) {
	raw := `{"steps":[{"key":"a"},{"key":"b","depends_on":["a","a","a"]}]}`
	d, err := ParseDecision(raw)
	if err != nil {
		t.Fatalf("应解析成功，实际错误：%v", err)
	}
	if got := d.Steps[1].DependsOn; len(got) != 1 || got[0] != "a" {
		t.Fatalf("依赖应去重为 [a]，实际 %v", got)
	}
}

// TestParseDecisionErrors 覆盖各类解析/校验失败，断言中文错误关键字。
func TestParseDecisionErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "空字符串", raw: "", want: "模型输出为空"},
		{name: "仅空白", raw: "  \n\t ", want: "模型输出为空"},
		{name: "没有任何 JSON", raw: "这一轮我先想想，暂时没有结论。", want: "未找到 JSON 对象"},
		{name: "缺少闭合花括号", raw: `{"steps":[`, want: "不完整"},
		{name: "字符串未闭合", raw: `{"summary":"没有收尾引号}`, want: "不完整"},
		{name: "JSON 语法错误", raw: fence(`{"steps":}`), want: "解析决策 JSON 失败"},
		{name: "围栏未闭合", raw: "```json\n{\"steps\":[]}\n", want: "围栏未闭合"},
		{name: "围栏内容为空", raw: "```json\n\n```", want: "围栏内容为空"},
		{name: "JSON 数组而不是对象", raw: `[{"key":"a"}]`, want: "JSON 数组"},
		{name: "围栏内是 JSON 数组", raw: fence(`[{"key":"a"}]`), want: "JSON 数组"},
		{name: "校验失败：重复 key", raw: `{"steps":[{"key":"a"},{"key":"a"}]}`, want: "决策校验失败"},
		{name: "校验失败：非法 kind", raw: `{"steps":[{"key":"a"}],"actions":[{"step_key":"a","kind":"hack"}]}`, want: "决策校验失败"},
		{name: "校验失败：依赖环", raw: `{"steps":[{"key":"a","depends_on":["b"]},{"key":"b","depends_on":["a"]}]}`, want: "依赖环"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseDecision(c.raw)
			if err == nil {
				t.Fatal("应返回错误，实际解析成功")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应包含 %q，实际为 %q", c.want, err.Error())
			}
		})
	}
}

// TestParseDecisionSingleLineFenceMissingClose 校验单行围栏漏掉闭围栏时
// 报「未闭合」而不是误解析。
func TestParseDecisionSingleLineFenceMissingClose(t *testing.T) {
	_, err := ParseDecision("```json {\"steps\":[]}")
	if err == nil || !strings.Contains(err.Error(), "围栏未闭合") {
		t.Fatalf("应提示围栏未闭合，实际错误：%v", err)
	}
}

func TestParseDecisionWithKnownAllowsExistingAction(t *testing.T) {
	raw := `{"actions":[{"step_key":"recon","kind":"probe","detail":"确认可达"}],"done":false}`
	if _, err := ParseDecision(raw); err == nil {
		t.Fatal("未提供已知步骤时，引用已有步骤的动作应被拒绝")
	}
	dec, err := ParseDecisionWithKnown(raw, map[string]bool{"recon": true})
	if err != nil {
		t.Fatalf("提供已知步骤后应解析成功: %v", err)
	}
	if len(dec.Actions) != 1 || dec.Actions[0].StepKey != "recon" {
		t.Fatalf("解析结果不符: %+v", dec)
	}
}
