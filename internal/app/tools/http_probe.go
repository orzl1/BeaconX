package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/policy"
)

const maxToolOutput = 32 << 10

// HTTPProbe 发送一次受限的 HTTP 请求并返回状态、响应头与正文片段。
type HTTPProbe struct {
	client *http.Client
}

// NewHTTPProbe 创建 HTTP 探测工具。
func NewHTTPProbe() *HTTPProbe {
	return &HTTPProbe{client: &http.Client{Timeout: 30 * time.Second}}
}

type httpProbeArgs struct {
	URL            string            `json:"url"`
	Method         string            `json:"method,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           string            `json:"body,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// Definition 实现 Tool。
func (*HTTPProbe) Definition() Definition {
	return Definition{
		Name:        "http_probe",
		Description: "向目标发送一次 HTTP 请求，返回状态码、响应头与正文片段（最多 32KB）。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "完整 URL（仅支持 http/https）"},
    "method": {"type": "string", "description": "HTTP 方法，默认 GET"},
    "headers": {"type": "object", "description": "附加请求头"},
    "body": {"type": "string", "description": "请求体，可选"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数（1-60）"}
  },
  "required": ["url"]
}`),
		Risk:  policy.RiskMedium,
		Roles: []string{"worker"},
	}
}

// Execute 实现 Tool。
func (t *HTTPProbe) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args httpProbeArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return Result{}, fmt.Errorf("参数解析失败: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(args.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Result{}, fmt.Errorf("url 必须是合法的 http/https 地址")
	}
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}
	timeout := time.Duration(args.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 60*time.Second {
		timeout = 15 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, u.String(), strings.NewReader(args.Body))
	if err != nil {
		return Result{}, fmt.Errorf("构造请求失败: %w", err)
	}
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxToolOutput))

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d\n", resp.StatusCode)
	for _, key := range []string{"Content-Type", "Server", "Location", "Content-Length"} {
		if v := resp.Header.Get(key); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", key, v)
		}
	}
	b.WriteString("\n")
	b.Write(body)
	return Result{
		Output: b.String(),
		Meta:   map[string]any{"status": resp.StatusCode, "url": u.String(), "method": method},
	}, nil
}
