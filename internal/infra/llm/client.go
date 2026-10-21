// Package llm 提供最小的 OpenAI 兼容 Chat Completions 客户端。
//
// 只依赖标准库，供 DeepSeek / OpenAI 等兼容端点使用。设计约束：
//   - 同一 Client 可并发复用：除构造期确定的配置外不保存请求级状态；
//   - 错误信息中不得出现 API Key：HTTP 错误体在构造 *APIError 之前完成脱敏；
//   - 除显式重试外不做任何网络访问（不探测模型列表、不上报 telemetry）；
//   - 所有错误文本使用中文，便于日志与前端提示统一展示。
package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 默认值与内部限制。
const (
	// defaultMaxTokens 是 Config.MaxTokens <= 0 时的输出上限。
	defaultMaxTokens = 2048
	// defaultTimeout 是 Config.Timeout <= 0 时的单次请求超时。
	defaultTimeout = 60 * time.Second
	// defaultMaxRetries 是 Config.MaxRetries <= 0 时的重试次数（不含首次请求）。
	defaultMaxRetries = 2
	// retryBaseDelay 是指数退避的基准间隔：100ms、200ms、400ms……
	retryBaseDelay = 100 * time.Millisecond
	// maxRetryShift 限制退避的位移量，避免异常配置把时间间隔算到溢出。
	maxRetryShift = 20
)

// Config 是 OpenAI 兼容接口的连接配置。
type Config struct {
	// BaseURL 是接口根地址，例如 https://api.deepseek.com 或 https://api.openai.com/v1。
	// 末尾斜杠会被去掉，请求最终发往 {BaseURL}/chat/completions。
	BaseURL string
	// APIKey 作为 Bearer Token 发送；为空时不发送 Authorization 头，
	// 以便在本地假服务或免鉴权网关上使用。
	APIKey string
	// Model 是必填的模型名。
	Model string
	// MaxTokens 是单次回复的输出上限；<= 0 时使用 2048。
	MaxTokens int
	// Temperature 仅在非 nil 时随请求发送。
	Temperature *float64
	// Timeout 是单次 HTTP 请求的超时；<= 0 时使用 60s。
	// 整个重试过程的耗时还受调用方 ctx 约束。
	Timeout time.Duration
	// MaxRetries 是最多重试次数（不含首次请求），<= 0 时使用 2。
	// 只有 429、5xx 和网络错误会重试。
	MaxRetries int
}

// Message 是一条会话消息。Role 取值 system、user、assistant 或 tool。
//
// 工具调用相关字段保持向后兼容：普通文本消息不设置 ToolCalls/ToolCallID 时，
// 序列化结果与扩展前完全一致。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls 用于 Role="assistant" 的消息：把模型上一轮发起的工具调用
	// 原样回传给接口，接口据此把工具结果与调用关联起来。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 用于 Role="tool" 的消息：指明这条结果是哪一次工具调用的返回值。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// Usage 是服务端返回的 token 统计。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response 是一次补全调用解析后的结果。
type Response struct {
	Content      string
	Usage        Usage
	Model        string
	FinishReason string
}

// APIError 表示服务端返回了非 2xx 状态码。
//
// Body 是截断并脱敏后的响应体，保证不包含 API Key，
// 便于直接展示或写日志；StatusCode 用于调用方分类处理；
// Kind 见同包的 ClassifyError / APIError.Kind。
type APIError struct {
	StatusCode int
	Body       string

	// kind 在构造时按状态码与 Body 关键词预分类；为零值时由 Kind() 现场推断，
	// 因此手工构造的 &APIError{StatusCode: ..., Body: ...} 同样可正确分类。
	kind ErrorKind
}

// Error 实现 error。
func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("LLM 接口返回 HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("LLM 接口返回 HTTP %d：%s", e.StatusCode, e.Body)
}

// transportError 标记传输层错误（连接失败、超时、读响应中断），属于可重试类别。
type transportError struct {
	err error
}

// Error 实现 error。
func (e *transportError) Error() string { return e.err.Error() }

// Unwrap 支持 errors.Is / errors.As 穿透到原始错误。
func (e *transportError) Unwrap() error { return e.err }

// Client 是 OpenAI 兼容 Chat Completions 客户端。构造后可并发使用。
type Client struct {
	baseURL     string
	apiKey      string
	model       string
	maxTokens   int
	temperature *float64
	maxRetries  int
	http        *http.Client
}

// NewClient 校验配置并构造客户端。
//
// 校验项：base_url 必须是以 http:// 或 https:// 开头的可解析地址，model 不能为空。
// api_key 允许为空（本地假服务）；为空时不会发送 Authorization 头。
func NewClient(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("base_url 不能为空")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("解析 base_url 失败：%w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("base_url 必须以 http:// 或 https:// 开头")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("base_url 缺少主机名")
	}

	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, errors.New("model 不能为空")
	}

	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}

	var temperature *float64
	if cfg.Temperature != nil {
		v := *cfg.Temperature
		temperature = &v
	}

	return &Client{
		baseURL:     base,
		apiKey:      cfg.APIKey,
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		maxRetries:  maxRetries,
		http:        &http.Client{Timeout: timeout},
	}, nil
}

// Complete 发送一次普通 Chat Completions 请求。
func (c *Client) Complete(ctx context.Context, messages []Message) (Response, error) {
	return c.complete(ctx, messages, false)
}

// CompleteJSON 要求服务端以 JSON 对象形式返回内容：
// 请求体带 response_format={"type":"json_object"}，内容仍由调用方解析。
//
// 兼容性回退：部分网关不支持 response_format 并以 HTTP 400 拒绝请求，
// 此时自动回退到普通 Complete 再试一次。其余错误（鉴权失败、重试耗尽的
// 429/5xx）不触发回退，避免掩盖真实故障或翻倍消耗配额。
func (c *Client) CompleteJSON(ctx context.Context, messages []Message) (Response, error) {
	resp, err := c.complete(ctx, messages, true)
	if err == nil {
		return resp, nil
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
		resp, fallbackErr := c.Complete(ctx, messages)
		if fallbackErr != nil {
			return Response{}, fmt.Errorf("JSON 模式被服务端拒绝（HTTP 400），回退普通请求仍失败：%w", fallbackErr)
		}
		return resp, nil
	}
	return Response{}, err
}

// completion 是一次成功响应的原始解析结果：文本与工具调用可能并存，也可能只有其一。
type completion struct {
	content      string
	toolCalls    []ToolCall
	usage        Usage
	model        string
	finishReason string
}

// complete 是 Complete / CompleteJSON 的公共实现，按需插入 response_format。
func (c *Client) complete(ctx context.Context, messages []Message, jsonMode bool) (Response, error) {
	ctx = nonNilContext(ctx)
	if len(messages) == 0 {
		return Response{}, errors.New("messages 不能为空")
	}

	payload, err := encodeRequest(c, messages, jsonMode, nil, "")
	if err != nil {
		return Response{}, err
	}

	comp, err := c.exchange(ctx, payload)
	if err != nil {
		return Response{}, err
	}
	if strings.TrimSpace(comp.content) == "" {
		return Response{}, errors.New("LLM 响应内容为空")
	}
	return Response{
		Content:      comp.content,
		Usage:        comp.usage,
		Model:        comp.model,
		FinishReason: comp.finishReason,
	}, nil
}

// exchange 执行带重试的请求，返回原始解析结果。
// 重试分类与取消语义见 retryable / waitBackoff；Complete 与 CompleteWithTools 共用。
func (c *Client) exchange(ctx context.Context, payload []byte) (completion, error) {
	attempts := c.maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := waitBackoff(ctx, attempt); err != nil {
				return completion{}, err
			}
		}

		comp, err := c.do(ctx, payload)
		if err == nil {
			return comp, nil
		}
		// 调用方取消或超时优先于重试：不再消耗后备尝试与退避时间。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return completion{}, fmt.Errorf("调用 LLM 接口已取消：%w", ctxErr)
		}
		if attempt == attempts-1 || !retryable(err) {
			return completion{}, err
		}
	}
	// 理论上不可达：循环内一定会返回。
	return completion{}, errors.New("调用 LLM 接口失败：未知错误")
}

// nonNilContext 把 nil ctx 归一为 Background，避免下游 panic。
func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// retryable 判断错误是否属于可重试类别：429、5xx 与网络错误。
// 4xx（除 429）以及解析/超限类错误不重试。
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= http.StatusInternalServerError
	}
	var tr *transportError
	return errors.As(err, &tr)
}

// waitBackoff 按第 retry 次重试（从 1 开始）等待指数退避，并尊重 ctx。
func waitBackoff(ctx context.Context, retry int) error {
	delay := retryBaseDelay
	if shift := retry - 1; shift > 0 {
		if shift > maxRetryShift {
			shift = maxRetryShift
		}
		delay <<= shift
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("等待重试时被取消：%w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
