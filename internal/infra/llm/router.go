package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Profile 描述 Router 中一个可用的 LLM 端点。
type Profile struct {
	// Name 是日志与 Profiles() 使用的可读名称；为空时自动生成 profile-<序号>。
	Name string
	// BaseURL / APIKey / Model / MaxTokens 透传给内部的 Client 配置。
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
}

// Router 在多个 LLM profile 之间做单层故障转移。
//
// 设计要点：
//   - 只在当前 profile 返回可切换类别的错误时前进到下一个（见 switchable）；
//   - 每个 Client 仍保留自己的内部重试与超时语义，Router 不重复实现退避；
//   - 构造后不可变，Client 本身并发安全，因此 Router 可并发使用；
//   - 不修改调用方传入的 messages/tools（内部 Client 只读）。
type Router struct {
	profiles []routedProfile
	log      *slog.Logger
}

// routedProfile 是一个已构建好的 profile（名称 + 客户端）。
type routedProfile struct {
	name   string
	client *Client
}

// NewRouter 校验并构建路由。
//
// 校验规则：至少一个 profile；每个 profile 的 base_url 必须可解析且以 http(s) 开头，
// model 必须非空（由 NewClient 统一校验）。timeout/maxRetries 直接透传给每个 Client，
// 其 <=0 的默认值语义与 Client 配置一致。log 可以为 nil（不写切换日志）。
func NewRouter(profiles []Profile, timeout time.Duration, maxRetries int, log *slog.Logger) (*Router, error) {
	if len(profiles) == 0 {
		return nil, errors.New("LLM 路由至少需要一个 profile")
	}

	entries := make([]routedProfile, 0, len(profiles))
	for i, profile := range profiles {
		name := strings.TrimSpace(profile.Name)
		if name == "" {
			name = fmt.Sprintf("profile-%d", i+1)
		}
		client, err := NewClient(Config{
			BaseURL:    profile.BaseURL,
			APIKey:     profile.APIKey,
			Model:      profile.Model,
			MaxTokens:  profile.MaxTokens,
			Timeout:    timeout,
			MaxRetries: maxRetries,
		})
		if err != nil {
			return nil, fmt.Errorf("第 %d 个 LLM profile（%s）配置无效：%w", i+1, name, err)
		}
		entries = append(entries, routedProfile{name: name, client: client})
	}

	return &Router{profiles: entries, log: log}, nil
}

// Profiles 返回按尝试顺序排列的 profile 名称副本。
func (r *Router) Profiles() []string {
	names := make([]string, len(r.profiles))
	for i := range r.profiles {
		names[i] = r.profiles[i].name
	}
	return names
}

// Complete 按 profile 顺序执行普通补全。
func (r *Router) Complete(ctx context.Context, messages []Message) (Response, error) {
	var out Response
	err := r.route(ctx, func(_ string, client *Client) error {
		var callErr error
		out, callErr = client.Complete(ctx, messages)
		return callErr
	})
	if err != nil {
		return Response{}, err
	}
	return out, nil
}

// CompleteJSON 按 profile 顺序执行 JSON 模式补全（response_format 回退语义见 Client）。
func (r *Router) CompleteJSON(ctx context.Context, messages []Message) (Response, error) {
	var out Response
	err := r.route(ctx, func(_ string, client *Client) error {
		var callErr error
		out, callErr = client.CompleteJSON(ctx, messages)
		return callErr
	})
	if err != nil {
		return Response{}, err
	}
	return out, nil
}

// CompleteWithTools 按 profile 顺序执行带工具定义的补全。
func (r *Router) CompleteWithTools(ctx context.Context, messages []Message, tools []ToolDef, toolChoice string) (ToolResponse, error) {
	var out ToolResponse
	err := r.route(ctx, func(_ string, client *Client) error {
		var callErr error
		out, callErr = client.CompleteWithTools(ctx, messages, tools, toolChoice)
		return callErr
	})
	if err != nil {
		return ToolResponse{}, err
	}
	return out, nil
}

// route 是三个公开方法的公共路由循环。
//
// 返回语义：
//   - 某个 profile 成功 → nil；
//   - 遇到不可切换类别（context_overflow/fatal）→ 原样返回该错误（保留 *APIError）；
//   - 全部失败 → *RouteError，Unwrap 指向最后错误，并记录最终类别。
func (r *Router) route(ctx context.Context, call func(profileName string, client *Client) error) error {
	ctx = nonNilContext(ctx)

	attempted := 0
	var lastErr error
	lastKind := KindFatal

	for i := range r.profiles {
		// 调用方已取消/超时：不再浪费后续 profile。
		if ctx.Err() != nil {
			break
		}

		attempted++
		err := call(r.profiles[i].name, r.profiles[i].client)
		if err == nil {
			return nil
		}

		lastErr = err
		lastKind = ClassifyError(err)
		if !switchable(lastKind) {
			// 确定性错误：换 profile 也是同样结果，立即失败。
			return err
		}
		if i == len(r.profiles)-1 || ctx.Err() != nil {
			break
		}
		// 每个切换决策只写一条日志；Client 内部重试不会经过这里。
		r.logSwitch(r.profiles[i].name, r.profiles[i+1].name, lastKind, err)
	}

	if lastErr == nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("LLM 路由已取消：%w", ctxErr)
		}
		return &RouteError{Attempts: attempted, Kind: KindFatal}
	}
	return &RouteError{Attempts: attempted, Kind: lastKind, Last: lastErr}
}

// logSwitch 写一条结构化切换日志；logger 为 nil 时静默。
func (r *Router) logSwitch(from, to string, kind ErrorKind, cause error) {
	if r.log == nil {
		return
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	r.log.Warn("模型切换",
		"from", from,
		"to", to,
		"kind", string(kind),
		"reason", reason,
	)
}
