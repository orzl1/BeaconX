package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// ToolDef 描述一个可供模型调用的函数工具。
// Parameters 是 JSON Schema 原文，客户端不做解析，原样透传给服务端。
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall 是模型发起的一次工具调用。
// Arguments 是模型生成的原始 JSON 字符串（无参数工具可能为空），由调用方解析与校验。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolMessage 描述一条回灌给模型的工具执行结果。
//
// 它只是数据形状的便捷定义：实际调用 CompleteWithTools 时请构造
// Message{Role: "tool", ToolCallID: ..., Content: ...}。
type ToolMessage struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// ToolResponse 是 CompleteWithTools 的结果。
// Content 与 ToolCalls 可能并存：Content 是模型附带的说明文本，允许为空。
type ToolResponse struct {
	Content      string
	ToolCalls    []ToolCall
	Usage        Usage
	Model        string
	FinishReason string
}

// CompleteWithTools 发送 tools 定义并解析「文本 + tool_calls」响应。
//
// 消息约定：
//   - 普通轮次使用 Role=user/system 的 Message；
//   - 把上一轮模型发起的调用回传时，构造 Role=assistant 且带 ToolCalls 的 Message；
//   - 回灌工具结果时，构造 Role=tool、ToolCallID 为对应调用 id 的 Message。
//
// tools 为空时不发送 tools 字段；toolChoice 为空时不发送 tool_choice 字段
// （常用取值为 "auto"/"none"）。端点、认证、8MB 限制、429/5xx/网络错误重试、
// ctx 取消、超时与密钥脱敏均与 Complete 共用同一条链路，语义完全一致。
func (c *Client) CompleteWithTools(ctx context.Context, messages []Message, tools []ToolDef, toolChoice string) (ToolResponse, error) {
	ctx = nonNilContext(ctx)
	if len(messages) == 0 {
		return ToolResponse{}, errors.New("messages 不能为空")
	}

	payload, err := encodeRequest(c, messages, false, tools, toolChoice)
	if err != nil {
		return ToolResponse{}, err
	}

	comp, err := c.exchange(ctx, payload)
	if err != nil {
		return ToolResponse{}, err
	}
	// 工具调用场景下「只有 tool_calls、没有正文」是合法响应；
	// 两者都为空说明模型什么都没返回，按错误处理。
	if strings.TrimSpace(comp.content) == "" && len(comp.toolCalls) == 0 {
		return ToolResponse{}, errors.New("LLM 响应既没有内容也没有 tool_calls")
	}

	return ToolResponse{
		Content:      comp.content,
		ToolCalls:    comp.toolCalls,
		Usage:        comp.usage,
		Model:        comp.model,
		FinishReason: comp.finishReason,
	}, nil
}
