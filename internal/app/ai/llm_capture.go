package ai

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// LLM 请求/响应正文采集（B7）。
//
// 采集发生在 AI 层（planner/worker 调用完模型之后），不改 llm 客户端：
//   - 每次 CompleteJSON / CompleteWithTools 调用写一条 llm_exchanges；
//   - messages 先序列化成 JSON，再用 evidence.RedactJSON 对敏感键（token /
//     password / secret 等）脱敏；模型响应若是 JSON 也做同样脱敏，否则按常见
//     密钥形态（Bearer / sk- / api_key=…）做文本脱敏；
//   - 单条正文按 UTF-8 字节安全截断到 64KB，并记录 truncated 标记；
//   - 开关 BEACONX_LLM_CAPTURE（默认 true，false/0/off 关闭）；
//   - 写入失败只 Warn，绝不影响规划/执行主流程。

// maxCapturedBytes 是单条请求/响应正文的上限（UTF-8 字节）。
const maxCapturedBytes = 64 * 1024

// maxCapturedErrorBytes 是错误文本的上限（错误只用于排障，无需完整保存）。
const maxCapturedErrorBytes = 2000

// captureEnvKey 是正文采集开关的环境变量名。
const captureEnvKey = "BEACONX_LLM_CAPTURE"

// captureEnabled 读取采集开关。默认开启；明确写 false/0/no/off 才关闭。
//
// 每次调用都读环境变量：配置是进程级的，但测试需要在同一进程里切换开关。
func captureEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(captureEnvKey))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// capturedMessage 是采集视图中的一条消息。
//
// 与 llm.Message 的区别：tool_calls.arguments 保存为**原始 JSON 值**而不是
// 字符串，这样 RedactJSON 才能看到参数对象内部的敏感键并脱敏。
type capturedMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	ToolCalls  []capturedToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

// capturedToolCall 是采集视图中的一次工具调用。
type capturedToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// captureMessages 把 llm.Message 转成可脱敏的采集视图。
func captureMessages(messages []llm.Message) []capturedMessage {
	out := make([]capturedMessage, 0, len(messages))
	for _, message := range messages {
		view := capturedMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			arguments := json.RawMessage(strings.TrimSpace(call.Arguments))
			if len(arguments) == 0 || !json.Valid(arguments) {
				// 参数不是合法 JSON 时按字符串保存（仍会走文本级脱敏路径之外的键脱敏）。
				quoted, err := json.Marshal(call.Arguments)
				if err != nil {
					quoted = []byte(`""`)
				}
				arguments = quoted
			}
			view.ToolCalls = append(view.ToolCalls, capturedToolCall{
				ID: call.ID, Name: call.Name, Arguments: arguments,
			})
		}
		out = append(out, view)
	}
	return out
}

// captureToolCalls 把工具调用转成采集视图（用于「只有工具调用、没有文本」的响应）。
func captureToolCalls(calls []llm.ToolCall) []capturedToolCall {
	out := make([]capturedToolCall, 0, len(calls))
	for _, call := range calls {
		arguments := json.RawMessage(strings.TrimSpace(call.Arguments))
		if len(arguments) == 0 || !json.Valid(arguments) {
			quoted, err := json.Marshal(call.Arguments)
			if err != nil {
				quoted = []byte(`""`)
			}
			arguments = quoted
		}
		out = append(out, capturedToolCall{ID: call.ID, Name: call.Name, Arguments: arguments})
	}
	return out
}

// buildCapturedRequest 生成已脱敏、已截断的请求 JSON。
func buildCapturedRequest(messages []llm.Message) (request []byte, size int, truncated bool) {
	raw, err := json.Marshal(map[string]any{"messages": captureMessages(messages)})
	if err != nil {
		return []byte(`{"_capture_error":"消息序列化失败"}`), 0, false
	}
	if redacted, redactErr := evidence.RedactJSON(raw); redactErr == nil {
		raw = redacted
	}
	if len(raw) <= maxCapturedBytes {
		return raw, len(raw), false
	}

	// 超限：按消息均分预算截断内容，保留结构可读性；再做一次脱敏。
	truncatedView, err := json.Marshal(map[string]any{
		"messages":        truncateCapturedMessages(messages, maxCapturedBytes),
		"_truncated":      true,
		"_original_bytes": len(raw),
	})
	if err != nil {
		return []byte(`{"_capture_error":"消息截断失败","_truncated":true}`), 0, true
	}
	if redacted, redactErr := evidence.RedactJSON(truncatedView); redactErr == nil {
		truncatedView = redacted
	}
	return truncatedView, len(truncatedView), true
}

// truncateCapturedMessages 按总预算截断每条消息的内容，保证 UTF-8 完整。
func truncateCapturedMessages(messages []llm.Message, budget int) []capturedMessage {
	views := captureMessages(messages)
	if len(views) == 0 {
		return views
	}
	// 预留结构开销（role/content/tool_calls 的键名与括号）。
	perMessage := budget / (len(views) + 2)
	if perMessage < 256 {
		perMessage = 256
	}
	for i := range views {
		views[i].Content = truncateBytes(views[i].Content, perMessage)
		for j := range views[i].ToolCalls {
			// 参数本身已是 JSON 片段：超限时退化为「带省略号的字符串值」。
			if len(views[i].ToolCalls[j].Arguments) > perMessage {
				quoted, err := json.Marshal(truncateBytes(string(views[i].ToolCalls[j].Arguments), perMessage))
				if err != nil {
					quoted = []byte(`"…(截断)"`)
				}
				views[i].ToolCalls[j].Arguments = quoted
			}
		}
	}
	return views
}

// buildCapturedResponse 生成已脱敏、已截断的响应文本。
//
// 只有工具调用没有文本时，把工具调用序列化成 {"tool_calls":[…]} 作为响应内容，
// 便于在「响应」端点里看到这一轮模型想做什么。
func buildCapturedResponse(content string, toolCalls []llm.ToolCall) (text string, truncated bool) {
	if strings.TrimSpace(content) == "" && len(toolCalls) > 0 {
		payload, err := json.Marshal(map[string]any{"tool_calls": captureToolCalls(toolCalls)})
		if err != nil {
			payload = []byte(`{"tool_calls":[]}`)
		}
		content = string(payload)
	}
	if strings.TrimSpace(content) == "" {
		return "", false
	}
	// JSON 响应按结构脱敏（能命中敏感键）；否则按文本形态脱敏。
	if json.Valid([]byte(content)) {
		if redacted, err := evidence.RedactJSON([]byte(content)); err == nil {
			content = string(redacted)
		}
	} else {
		content = redactFreeText(content)
	}
	if len(content) > maxCapturedBytes {
		return truncateBytes(content, maxCapturedBytes), true
	}
	return content, false
}

// freezeRedactPatterns 是文本级脱敏规则（用于非 JSON 响应与错误文本）。
var (
	bearerPattern  = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]{6,}`)
	skPattern      = regexp.MustCompile(`\bsk-[A-Za-z0-9._\-]{6,}`)
	apiKeyPattern  = regexp.MustCompile(`(?i)("?(?:api[_-]?key|access[_-]?token|token|password|secret)"?\s*[:=]\s*"?)[^\s",}]{6,}`)
	urlUserPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`)
)

// redactFreeText 对自由文本做密钥形态脱敏（JSON 结构脱敏覆盖不到的场景）。
func redactFreeText(text string) string {
	text = bearerPattern.ReplaceAllString(text, "${1}***")
	text = skPattern.ReplaceAllString(text, "sk-***")
	text = apiKeyPattern.ReplaceAllString(text, "${1}***")
	text = urlUserPattern.ReplaceAllString(text, "${1}***@")
	return text
}

// truncateBytes 按字节上限截断字符串，保证不把 UTF-8 字符截成半个。
func truncateBytes(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…(截断)"
}

// captureLLMExchange 记录一次模型调用的正文与用量。
//
// 任何失败都只打 Warn：采集是附加能力，不能反过来影响规划/执行。
func captureLLMExchange(ctx context.Context, store *pg.Store, log *slog.Logger,
	tenantID, missionID int64, role, model string,
	messages []llm.Message, content string, toolCalls []llm.ToolCall,
	usage llm.Usage, callErr error) {
	if store == nil || !captureEnabled() {
		return
	}
	request, requestBytes, truncated := buildCapturedRequest(messages)
	response, responseTruncated := buildCapturedResponse(content, toolCalls)
	var errText string
	if callErr != nil {
		errText = truncateBytes(redactFreeText(callErr.Error()), maxCapturedErrorBytes)
	}
	if _, err := store.RecordLLMExchange(ctx, tenantID, pg.LLMExchangeInput{
		MissionID:        missionID,
		Role:             role,
		Model:            model,
		Request:          request,
		Response:         response,
		Error:            errText,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		RequestBytes:     requestBytes,
		ResponseBytes:    len(response),
		Truncated:        truncated || responseTruncated,
		Redacted:         true,
	}); err != nil {
		if log != nil {
			log.Warn("记录 LLM 调用正文失败", "mission_id", missionID, "role", role, "error", err)
		}
	}
}

// captureExchange 记录一次 planner 的 CompleteJSON 调用（含失败调用）。
func (p *Planner) captureExchange(ctx context.Context, tenantID, missionID int64, role string, messages []llm.Message, resp llm.Response, callErr error) {
	captureLLMExchange(ctx, p.store, p.log, tenantID, missionID, role, p.model, messages,
		resp.Content, nil, resp.Usage, callErr)
}

// captureExchange 记录一次 worker 的 CompleteJSON 调用（含失败调用）。
func (w *Worker) captureExchange(ctx context.Context, tenantID, missionID int64, role string, messages []llm.Message, resp llm.Response, callErr error) {
	captureLLMExchange(ctx, w.store, w.log, tenantID, missionID, role, w.model, messages,
		resp.Content, nil, resp.Usage, callErr)
}

// captureToolExchange 记录一次 worker 的 CompleteWithTools 调用（含失败调用）。
func (w *Worker) captureToolExchange(ctx context.Context, tenantID, missionID int64, role string, messages []llm.Message, resp llm.ToolResponse, callErr error) {
	captureLLMExchange(ctx, w.store, w.log, tenantID, missionID, role, w.model, messages,
		resp.Content, resp.ToolCalls, resp.Usage, callErr)
}
