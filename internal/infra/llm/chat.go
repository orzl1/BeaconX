package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// 请求/响应的大小与展示限制。
const (
	// maxBodyBytes 同时限制请求体与响应体，防止异常数据打爆内存。
	maxBodyBytes = 8 << 20 // 8 MiB
	// errorBodyLimit 是塞进错误信息/APIError.Body 的错误体上限。
	errorBodyLimit = 2048
	// chatCompletionsPath 是 OpenAI 兼容端点路径，拼接在 BaseURL 之后。
	chatCompletionsPath = "/chat/completions"
)

// chatRequest 是发往 {base}/chat/completions 的请求体。
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	MaxTokens      int             `json:"max_tokens"`
	Temperature    *float64        `json:"temperature,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

// responseFormat 是 JSON 模式的结构化输出声明。
type responseFormat struct {
	Type string `json:"type"`
}

// chatResponse 是兼容 /chat/completions 的最小响应子集。
type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// encodeRequest 把配置与消息序列化为请求体，并检查大小上限。
func encodeRequest(c *Client, messages []Message, jsonMode bool) ([]byte, error) {
	req := chatRequest{
		Model:       c.model,
		Messages:    messages,
		MaxTokens:   c.maxTokens,
		Temperature: c.temperature,
	}
	if jsonMode {
		req.ResponseFormat = &responseFormat{Type: "json_object"}
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化 LLM 请求失败：%w", err)
	}
	if len(payload) > maxBodyBytes {
		return nil, fmt.Errorf("LLM 请求体超过 %d 字节限制", maxBodyBytes)
	}
	return payload, nil
}

// do 执行一次 HTTP 请求并解析结果。
//
// 返回的错误分为三类：
//   - *APIError：拿到非 2xx 状态码（Body 已脱敏、截断）；
//   - *transportError：连接/超时/读响应失败，可重试；
//   - 其它：响应格式错误、大小超限、空内容等，不重试。
func (c *Client) do(ctx context.Context, payload []byte) (Response, error) {
	endpoint := c.baseURL + chatCompletionsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("构造 LLM 请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "BeaconX-llm-client")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("请求 LLM 接口失败：%w", &transportError{err: err})
	}
	defer httpResp.Body.Close()

	body, err := readResponseBody(httpResp.Body)
	if err != nil {
		return Response{}, err
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode > 299 {
		return Response{}, &APIError{
			StatusCode: httpResp.StatusCode,
			Body:       sanitizeErrorBody(body, c.apiKey),
		}
	}
	return decodeResponse(body)
}

// readResponseBody 读取响应体并执行大小限制。
func readResponseBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 LLM 响应失败：%w", &transportError{err: err})
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("LLM 响应体超过 %d 字节限制", maxBodyBytes)
	}
	return body, nil
}

// decodeResponse 解析 choices[0].message.content、usage 与 finish_reason。
func decodeResponse(body []byte) (Response, error) {
	var wire chatResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return Response{}, fmt.Errorf("解析 LLM 响应失败：%w", err)
	}
	if len(wire.Choices) == 0 {
		return Response{}, errors.New("LLM 响应中没有 choices")
	}

	choice := wire.Choices[0]
	if strings.TrimSpace(choice.Message.Content) == "" {
		return Response{}, errors.New("LLM 响应内容为空")
	}

	return Response{
		Content:      choice.Message.Content,
		Usage:        wire.Usage,
		Model:        wire.Model,
		FinishReason: choice.FinishReason,
	}, nil
}

// sanitizeErrorBody 把错误响应体处理为可安全展示的文本：
// 去掉首尾空白、抹掉 API Key、截断到 errorBodyLimit 并保证 UTF-8 合法。
func sanitizeErrorBody(body []byte, apiKey string) string {
	text := strings.TrimSpace(string(body))
	if apiKey != "" {
		text = strings.ReplaceAll(text, apiKey, "[已隐藏]")
	}
	return truncateUTF8(text, errorBodyLimit)
}

// truncateUTF8 在不超过 max 字节的前提下截断字符串，且不切断多字节字符。
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	end := max
	for end > 0 && !utf8.ValidString(s[:end]) {
		end--
	}
	return s[:end] + "…（已截断）"
}
