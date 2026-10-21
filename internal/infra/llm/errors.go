package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrorKind 是 LLM 调用失败的结构化类别。
//
// 路由层据此决定是否值得切换到下一个 profile：只有 auth/quota/rate_limit/transient
// 可切换；context_overflow 与 fatal 立即失败，避免用其他 profile 重复撞同样的错误。
type ErrorKind string

const (
	// KindAuth：401/403，密钥无效或权限不足。
	KindAuth ErrorKind = "auth"
	// KindQuota：402 或余额不足。
	KindQuota ErrorKind = "quota"
	// KindRateLimit：429 限流。
	KindRateLimit ErrorKind = "rate_limit"
	// KindContextOverflow：400 且响应体提示上下文/长度超限。
	KindContextOverflow ErrorKind = "context_overflow"
	// KindTransient：5xx、网络错误或超时。
	KindTransient ErrorKind = "transient"
	// KindFatal：其他 4xx 或无法分类（含 nil 错误）。
	KindFatal ErrorKind = "fatal"
)

// contextOverflowMarkers 是判定 400 响应是否属于上下文超限的关键词（不区分大小写）。
// 只对 HTTP 400 生效：其他状态码优先按状态码分类。
var contextOverflowMarkers = []string{
	"context length",
	"maximum context",
	"context_length_exceeded",
	"context window",
	"too many tokens",
	"tokens exceed",
	"prompt is too long",
	"input is too long",
	"reduce the length",
	"上下文长度",
	"超过最大上下文",
}

// ClassifyError 把任意 LLM 调用错误归类为 ErrorKind。
//
// 分类顺序与规则：
//  1. nil → KindFatal（无法分类）；
//  2. context.DeadlineExceeded（含包装）→ KindTransient；context.Canceled → KindFatal
//     （调用方已主动取消，不应再切换 profile）；
//  3. *RouteError → 其记录的最终类别；
//  4. *APIError → 按 HTTP 状态码与 400 关键词分类（APIError.Kind）；
//  5. 传输层错误（*transportError、net.Error）→ KindTransient；
//  6. 其余 → KindFatal。
//
// 分类只读取错误的类型/状态码/已脱敏的响应体，不改变任何错误文本。
func ClassifyError(err error) ErrorKind {
	if err == nil {
		return KindFatal
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTransient
	}
	if errors.Is(err, context.Canceled) {
		return KindFatal
	}

	var routeErr *RouteError
	if errors.As(err, &routeErr) && routeErr.Kind != "" {
		return routeErr.Kind
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Kind()
	}

	var tr *transportError
	if errors.As(err, &tr) {
		return KindTransient
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return KindTransient
	}

	return KindFatal
}

// Kind 返回该 APIError 的结构化类别。
//
// 客户端构造 APIError 时已预分类；手工构造的 APIError 会在此现场按
// StatusCode + Body 推断，因此两种来源结果一致。
func (e *APIError) Kind() ErrorKind {
	if e == nil {
		return KindFatal
	}
	if e.kind != "" {
		return e.kind
	}
	return classifyAPIError(e.StatusCode, e.Body)
}

// classifyAPIError 按 HTTP 状态码与响应体关键词分类。
func classifyAPIError(statusCode int, body string) ErrorKind {
	switch {
	case statusCode == 401 || statusCode == 403:
		return KindAuth
	case statusCode == 402:
		return KindQuota
	case statusCode == 429:
		return KindRateLimit
	case statusCode >= 500:
		return KindTransient
	case statusCode == 400:
		if isContextOverflowBody(body) {
			return KindContextOverflow
		}
		return KindFatal
	default:
		return KindFatal
	}
}

// isContextOverflowBody 判断 400 响应体是否包含上下文超限提示。
func isContextOverflowBody(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// switchable 报告该类别是否值得切换到下一个 profile。
func switchable(kind ErrorKind) bool {
	switch kind {
	case KindAuth, KindQuota, KindRateLimit, KindTransient:
		return true
	default:
		return false
	}
}

// newAPIError 由原始响应体构造带分类的 APIError：
// 先脱敏 + 截断得到可展示的 Body（保持既有错误文本行为），再预分类。
func newAPIError(statusCode int, body []byte, apiKey string) *APIError {
	sanitized := sanitizeErrorBody(body, apiKey)
	return &APIError{
		StatusCode: statusCode,
		Body:       sanitized,
		kind:       classifyAPIError(statusCode, sanitized),
	}
}

// RouteError 表示 Router 在尝试若干 profile 后仍然失败。
//
// 它保留最后一个错误（Unwrap），因此 errors.Is/errors.As 仍能取到最后的
// *APIError；Kind 记录最后错误的结构化类别，供 ClassifyError 直接返回。
// Error 文本包含尝试过的 profile 数量与最后错误，但不含任何密钥。
type RouteError struct {
	Attempts int       // 实际尝试过的 profile 数量
	Kind     ErrorKind // 最后错误的类别
	Last     error     // 最后一个错误；可能为 nil（例如首个 profile 前 ctx 已取消）
}

// Error 实现 error。
func (e *RouteError) Error() string {
	if e == nil {
		return "LLM 路由失败"
	}
	if e.Last != nil {
		return fmt.Sprintf("LLM 路由失败：已尝试 %d 个 profile，最后错误：%v", e.Attempts, e.Last)
	}
	return fmt.Sprintf("LLM 路由失败：已尝试 %d 个 profile", e.Attempts)
}

// Unwrap 暴露最后一个错误，供 errors.Is/errors.As 使用。
func (e *RouteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Last
}
