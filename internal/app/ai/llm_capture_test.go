package ai

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/orzl1/BeaconX/internal/infra/llm"
)

// 采集辅助函数的纯单元测试（不需要数据库）：脱敏、截断、开关。

func TestCaptureEnabled(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", true}, // 未设置 = 默认开启
		{"true", true},
		{"1", true},
		{"yes", true},
		{"false", false},
		{"FALSE", false},
		{"0", false},
		{"off", false},
		{"no", false},
		{"随便写的值", true}, // 未知值按默认（开启）
	}
	for _, tc := range cases {
		t.Run("值="+tc.value, func(t *testing.T) {
			t.Setenv(captureEnvKey, tc.value)
			if got := captureEnabled(); got != tc.want {
				t.Fatalf("captureEnabled(%q) = %v，期望 %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestBuildCapturedRequestRedactsToolArguments(t *testing.T) {
	messages := []llm.Message{
		{Role: "system", Content: "系统提示"},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []llm.ToolCall{{
				ID:        "call-1",
				Name:      "shell_exec",
				Arguments: `{"cmd":"id","password":"hunter2","api_key":"sk-abcdef123456"}`,
			}},
		},
	}
	request, size, truncated := buildCapturedRequest(messages)
	if truncated {
		t.Fatal("小请求不应被截断")
	}
	if size != len(request) || size == 0 {
		t.Fatalf("request_bytes = %d，len = %d", size, len(request))
	}
	if !json.Valid(request) {
		t.Fatalf("采集的请求不是合法 JSON: %s", request)
	}
	text := string(request)
	if strings.Contains(text, "hunter2") || strings.Contains(text, "sk-abcdef123456") {
		t.Fatalf("敏感值未被脱敏: %s", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("缺少脱敏占位: %s", text)
	}

	// 结构化断言：tool_calls 参数是 JSON 对象（而不是字符串），且密码被替换。
	var doc struct {
		Messages []struct {
			ToolCalls []struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request, &doc); err != nil {
		t.Fatalf("解析采集请求失败: %v", err)
	}
	if len(doc.Messages) != 2 || len(doc.Messages[1].ToolCalls) != 1 {
		t.Fatalf("messages 结构异常: %+v", doc.Messages)
	}
	args := doc.Messages[1].ToolCalls[0].Arguments
	if args["password"] != "[REDACTED]" || args["cmd"] != "id" {
		t.Fatalf("参数脱敏结果异常: %+v", args)
	}
}

func TestBuildCapturedRequestTruncatesLargeMessages(t *testing.T) {
	huge := strings.Repeat("长", 40000) // 120000 字节，明显超过 64KB
	messages := []llm.Message{
		{Role: "system", Content: "系统提示"},
		{Role: "user", Content: huge},
	}
	request, size, truncated := buildCapturedRequest(messages)
	if !truncated {
		t.Fatal("超大请求应标记 truncated")
	}
	if size != len(request) {
		t.Fatalf("size = %d，len = %d", size, len(request))
	}
	if size > maxCapturedBytes+4096 {
		t.Fatalf("截断后仍过大: %d 字节", size)
	}
	if !utf8.Valid(request) {
		t.Fatal("截断破坏了 UTF-8")
	}
	if !json.Valid(request) {
		t.Fatalf("截断后不是合法 JSON: %s", request[:200])
	}
	if !strings.Contains(string(request), `"_truncated":true`) {
		t.Fatalf("缺少 truncated 标记: %s", request[:200])
	}
	// 逐条断言：user 消息内容必须短于原始内容（截断生效）。
	var doc struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request, &doc); err != nil {
		t.Fatalf("解析截断结果失败: %v", err)
	}
	if len(doc.Messages) != 2 || len(doc.Messages[1].Content) >= len(huge) {
		t.Fatalf("超大内容未被截断: 采集 %d 字节，原始 %d 字节", len(doc.Messages[1].Content), len(huge))
	}
	if !strings.HasSuffix(doc.Messages[1].Content, "…(截断)") {
		t.Fatalf("截断缺少标记: %q", doc.Messages[1].Content[len(doc.Messages[1].Content)-12:])
	}
}

func TestBuildCapturedResponseRedacts(t *testing.T) {
	t.Run("JSON 响应按键脱敏", func(t *testing.T) {
		text, truncated := buildCapturedResponse(`{"fact":"ok","password":"hunter2"}`, nil)
		if truncated {
			t.Fatal("小响应不应截断")
		}
		if strings.Contains(text, "hunter2") || !strings.Contains(text, "[REDACTED]") {
			t.Fatalf("JSON 响应脱敏失败: %s", text)
		}
	})

	t.Run("文本响应按密钥形态脱敏", func(t *testing.T) {
		text, _ := buildCapturedResponse(
			"Authorization: Bearer abcdef123456\nkey=sk-live12345678\nsee https://user:pass@example.com/x", nil)
		for _, leaked := range []string{"abcdef123456", "sk-live12345678", "user:pass"} {
			if strings.Contains(text, leaked) {
				t.Fatalf("文本响应泄漏 %q: %s", leaked, text)
			}
		}
		if !strings.Contains(text, "***") {
			t.Fatalf("文本响应未脱敏: %s", text)
		}
	})

	t.Run("只有工具调用时序列化为响应", func(t *testing.T) {
		text, _ := buildCapturedResponse("", []llm.ToolCall{{
			ID: "c1", Name: "shell_exec", Arguments: `{"cmd":"id","token":"secret-token-123"}`,
		}})
		if strings.Contains(text, "secret-token-123") {
			t.Fatalf("工具参数未脱敏: %s", text)
		}
		var doc struct {
			ToolCalls []map[string]any `json:"tool_calls"`
		}
		if err := json.Unmarshal([]byte(text), &doc); err != nil {
			t.Fatalf("工具调用响应不是 JSON: %v（%s）", err, text)
		}
		if len(doc.ToolCalls) != 1 || doc.ToolCalls[0]["name"] != "shell_exec" {
			t.Fatalf("工具调用响应结构异常: %+v", doc.ToolCalls)
		}
	})

	t.Run("超大响应截断", func(t *testing.T) {
		text, truncated := buildCapturedResponse(strings.Repeat("响", 40000), nil)
		if !truncated {
			t.Fatal("超大响应应标记 truncated")
		}
		if len(text) > maxCapturedBytes+64 {
			t.Fatalf("截断后仍过大: %d", len(text))
		}
		if !utf8.ValidString(text) {
			t.Fatal("截断破坏 UTF-8")
		}
		if !strings.HasSuffix(text, "…(截断)") {
			t.Fatalf("缺少截断标记: %s", text[len(text)-20:])
		}
	})
}

func TestTruncateBytesKeepsUTF8(t *testing.T) {
	text := strings.Repeat("汉字", 10)
	cut := truncateBytes(text, 7) // 7 字节会切在第二个汉字中间
	if !utf8.ValidString(cut) {
		t.Fatalf("截断结果不是合法 UTF-8: %q", cut)
	}
	if !strings.HasPrefix(text, strings.TrimSuffix(cut, "…(截断)")) {
		t.Fatalf("截断没有保留前缀: %q", cut)
	}
	if truncateBytes("短文本", 100) != "短文本" {
		t.Fatal("未超限时不应改动内容")
	}
}

func TestCaptureLLMExchangeWithoutStoreIsNoop(t *testing.T) {
	t.Setenv(captureEnvKey, "true")
	// store 为 nil 时必须直接返回（采集失败不能影响主流程）。
	captureLLMExchange(nil, nil, nil, 1, 1, "planner", "model", nil, "", nil, llm.Usage{}, nil)
}
