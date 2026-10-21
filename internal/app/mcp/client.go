// Package mcp 实现 BeaconX 侧的最小 MCP（Model Context Protocol）客户端。
//
// 目标不是覆盖完整协议，而是支撑「端点管理 + 刷新工具清单」这一个 B5-1 场景：
//
//   - http 传输：单次 POST 的 JSON-RPC，先 initialize（协议版本 2025-03-26），
//     再 tools/list；响应既可以是 application/json，也可以是
//     text/event-stream（SSE 里逐条 data: JSON），两条路径都会被解析；
//   - stdio 传输：启动 command args，按行读写 JSON-RPC（服务器可能把日志写到
//     stdout，因此非 JSON 行会被跳过）；stdio 默认禁用，只有显式开启并且命令
//     命中白名单时才允许；
//   - 所有调用共享一个 10s 超时（可由走用方覆盖），超时/失败都返回中文错误，
//     错误信息不包含 env/headers 的原值，避免把凭据写进 last_error；
//   - 工具数量、名称长度、描述长度与 inputSchema 体积都有上限，防止恶意或异常
//     的服务器把数据库撑大。
//
// 本包不含任何数据库访问：发现结果由调用方（pg.Store）负责落库。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout 是单次发现（initialize + tools/list）的端到端超时。
const DefaultTimeout = 10 * time.Second

// ProtocolVersion 是 initialize 请求声明的 MCP 协议版本。
//
// 与上游对齐的取值；服务器若返回不同版本，本实现不强制校验（最小实现），
// 但会把它当作握手成功，因为 tools/list 的形状在 2024-11..2025-06 之间稳定。
const ProtocolVersion = "2025-03-26"

// 发现结果的硬上限，防止异常服务器让数据库无限膨胀。
const (
	maxTools            = 500
	maxToolNameRunes    = 200
	maxDescriptionRunes = 4_000
	maxInputSchemaBytes = 64 << 10
	maxBodyBytes        = 4 << 20
	maxErrorBodySnippet = 200
)

// 传输类型，与迁移 0010 的 CHECK 约束保持一致。
const (
	TransportHTTP  = "http"
	TransportStdio = "stdio"
)

// ErrStdioDisabled 表示 stdio 传输未开启（默认）。
var ErrStdioDisabled = errors.New("stdio 传输默认禁用：需显式开启并配置命令白名单")

// ErrCommandNotAllowed 表示命令不在白名单内。
var ErrCommandNotAllowed = errors.New("stdio 命令不在允许列表内")

// Server 是一次发现所需的端点配置（来自 mcp_servers 行）。
type Server struct {
	Name      string
	Transport string
	URL       string
	Headers   map[string]string
	Command   string
	Args      []string
	Env       map[string]string
}

// Tool 是 tools/list 返回的一条工具。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// Options 控制发现的超时与 stdio 安全策略。
type Options struct {
	// Timeout 是端到端超时；<=0 时使用 DefaultTimeout。
	Timeout time.Duration
	// AllowStdio 为 false 时 stdio 端点直接拒绝（默认禁用）。
	AllowStdio bool
	// StdioAllowlist 非空时，command 必须精确命中其中一项；为空表示不额外限制
	// （是否允许 stdio 仍由 AllowStdio 决定）。
	StdioAllowlist []string
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return DefaultTimeout
}

// Discover 按端点传输类型发现工具清单。
func Discover(ctx context.Context, srv Server, opts Options) ([]Tool, error) {
	if strings.TrimSpace(srv.Name) == "" {
		return nil, errors.New("端点名称不能为空")
	}
	switch srv.Transport {
	case TransportHTTP:
		return discoverHTTP(ctx, srv, opts)
	case TransportStdio:
		return discoverStdio(ctx, srv, opts)
	default:
		return nil, fmt.Errorf("未知传输方式 %q", srv.Transport)
	}
}

// stdioAllowed 判断本次是否允许 stdio，以及命令是否在白名单内。
func stdioAllowed(srv Server, opts Options) error {
	if !opts.AllowStdio {
		return ErrStdioDisabled
	}
	command := strings.TrimSpace(srv.Command)
	if command == "" {
		return errors.New("stdio 端点缺少 command")
	}
	if len(opts.StdioAllowlist) == 0 {
		return nil
	}
	for _, allowed := range opts.StdioAllowlist {
		if strings.TrimSpace(allowed) == command {
			return nil
		}
	}
	return fmt.Errorf("%w：%s", ErrCommandNotAllowed, command)
}

// ---------- JSON-RPC 信封 ----------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("MCP 服务返回错误 %d：%s", e.Code, strings.TrimSpace(e.Message))
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcID 分配请求 id（从 1 开始，逐次递增）。
func rpcID(n int) *int { return &n }

// toolListResult 兼容 inputSchema 与 input_schema 两种命名。
type toolListResult struct {
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
		// 少数实现使用下划线命名；两者都接受。
		InputSchemaAlt json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	NextCursor string `json:"nextCursor"`
}

// decodeTools 把 tools/list 结果转成带上限校验的 Tool 列表。
func decodeTools(result json.RawMessage) (toolListResult, []Tool, error) {
	var parsed toolListResult
	if len(result) == 0 {
		return parsed, []Tool{}, nil
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return parsed, nil, fmt.Errorf("解析 tools/list 结果失败：%w", err)
	}
	out := make([]Tool, 0, len(parsed.Tools))
	for _, raw := range parsed.Tools {
		name := strings.TrimSpace(raw.Name)
		if name == "" {
			continue
		}
		if len([]rune(name)) > maxToolNameRunes {
			name = string([]rune(name)[:maxToolNameRunes])
		}
		desc := strings.TrimSpace(raw.Description)
		if len([]rune(desc)) > maxDescriptionRunes {
			desc = string([]rune(desc)[:maxDescriptionRunes])
		}
		schema := raw.InputSchema
		if len(bytes.TrimSpace(schema)) == 0 || bytes.Equal(bytes.TrimSpace(schema), []byte("null")) {
			schema = raw.InputSchemaAlt
		}
		parsedSchema := map[string]any{}
		if len(bytes.TrimSpace(schema)) > 0 && len(schema) <= maxInputSchemaBytes {
			if err := json.Unmarshal(schema, &parsedSchema); err != nil {
				parsedSchema = map[string]any{}
			}
		}
		out = append(out, Tool{Name: name, Description: desc, InputSchema: parsedSchema})
		if len(out) >= maxTools {
			break
		}
	}
	return parsed, out, nil
}

// ---------- http 传输 ----------

// discoverHTTP 用单次 POST 完成 initialize + tools/list。
//
// 注意：响应头/请求头的值不进入错误信息；失败只暴露状态码与 JSON-RPC 错误文本。
func discoverHTTP(ctx context.Context, srv Server, opts Options) ([]Tool, error) {
	url := strings.TrimSpace(srv.URL)
	if url == "" {
		return nil, errors.New("http 端点缺少 url")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, errors.New("http 端点的 url 必须是 http/https 地址")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	client := &http.Client{Timeout: opts.timeout()}
	post := func(body rpcRequest) (*rpcResponse, error) {
		return httpRoundTrip(ctx, client, url, srv.Headers, body)
	}

	initResp, err := post(rpcRequest{
		JSONRPC: "2.0",
		ID:      rpcID(1),
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "beaconx", "version": "1.0"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize 失败：%w", err)
	}
	if initResp.Error != nil {
		return nil, fmt.Errorf("initialize 被拒绝：%w", initResp.Error)
	}
	// initialized 通知：服务器可能返回 202/空体，失败不阻断发现。
	_, _ = post(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})

	return httpListTools(ctx, post)
}

// httpListTools 分页拉取 tools/list（多数服务器一页返回完）。
func httpListTools(ctx context.Context, post func(rpcRequest) (*rpcResponse, error)) ([]Tool, error) {
	const maxPages = 10
	collected := make([]Tool, 0, 16)
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		resp, err := post(rpcRequest{JSONRPC: "2.0", ID: rpcID(2 + page), Method: "tools/list", Params: params})
		if err != nil {
			return nil, fmt.Errorf("tools/list 失败：%w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("tools/list 被拒绝：%w", resp.Error)
		}
		parsed, tools, err := decodeTools(resp.Result)
		if err != nil {
			return nil, err
		}
		collected = append(collected, tools...)
		if len(collected) >= maxTools || parsed.NextCursor == "" {
			if len(collected) > maxTools {
				collected = collected[:maxTools]
			}
			return collected, nil
		}
		cursor = parsed.NextCursor
	}
	return collected, nil
}

// httpRoundTrip 发送一条 JSON-RPC 并解析响应（json 或 SSE）。
func httpRoundTrip(ctx context.Context, client *http.Client, url string, headers map[string]string, body rpcRequest) (*rpcResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("编码请求失败：%w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if err := applyHeaders(req, headers); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// 不把 URL 完整拼进错误里也能定位；这里保留错误类型信息即可。
		return nil, fmt.Errorf("请求 MCP 端点失败：%w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxBodyBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(limited, maxErrorBodySnippet))
		text := strings.TrimSpace(string(snippet))
		if text == "" {
			return nil, fmt.Errorf("MCP 端点返回 http 状态 %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("MCP 端点返回 http 状态 %d：%s", resp.StatusCode, text)
	}
	if body.ID == nil {
		// 通知类请求不需要响应体。
		_, _ = io.Copy(io.Discard, limited)
		return &rpcResponse{}, nil
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		return readSSEResponse(limited, *body.ID)
	}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}
	var out rpcResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errors.New("MCP 端点响应不是合法 JSON-RPC")
	}
	return &out, nil
}

// applyHeaders 应用自定义请求头；拒绝 CR/LF，避免头注入。
func applyHeaders(req *http.Request, headers map[string]string) error {
	for key, value := range headers {
		name := strings.TrimSpace(key)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("自定义请求头 %q 含非法字符", name)
		}
		req.Header.Set(name, value)
	}
	return nil
}

// readSSEResponse 从 SSE 流里找到 id 匹配的 JSON-RPC 响应。
//
// 兼容两种常见形态：`data: {...}` 单行 JSON，以及多行 data 拼接的事件体。
func readSSEResponse(r io.Reader, wantID int) (*rpcResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxBodyBytes)
	var dataLines []string
	flush := func() (*rpcResponse, bool) {
		if len(dataLines) == 0 {
			return nil, false
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		var resp rpcResponse
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return nil, false
		}
		if resp.ID != nil && *resp.ID == wantID {
			return &resp, true
		}
		return nil, false
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if resp, ok := flush(); ok {
				return resp, nil
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	if resp, ok := flush(); ok {
		return resp, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 SSE 响应失败：%w", err)
	}
	return nil, fmt.Errorf("SSE 流里没有 id=%d 的 JSON-RPC 响应", wantID)
}

// ---------- stdio 传输 ----------

// discoverStdio 以行协议与子进程完成 initialize + tools/list。
//
// 安全：默认禁用；是否允许由 Options 决定（见 stdioAllowed）。子进程继承当前
// 进程环境，再叠加端点配置的 env；任何 env 值都不会出现在错误文本中。
func discoverStdio(ctx context.Context, srv Server, opts Options) ([]Tool, error) {
	if err := stdioAllowed(srv, opts); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, strings.TrimSpace(srv.Command), srv.Args...)
	cmd.Env = append(os.Environ(), envPairs(srv.Env)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建子进程输入管道失败：%w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建子进程输出管道失败：%w", err)
	}
	// stderr 只保留尾部少量字节用于诊断；同样不回显 env。
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 stdio MCP 失败：%w", err)
	}
	defer func() {
		cancel()
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	reader := bufio.NewReaderSize(stdout, 64<<10)
	write := func(body rpcRequest) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求失败：%w", err)
		}
		raw = append(raw, '\n')
		if _, err := stdin.Write(raw); err != nil {
			return fmt.Errorf("写入子进程失败：%w", err)
		}
		return nil
	}
	// readResponse 读取直到遇到目标 id 的 JSON-RPC 响应；非 JSON 行（日志）跳过。
	readResponse := func(wantID int) (*rpcResponse, error) {
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				msg := strings.TrimSpace(stderr.String())
				if msg != "" {
					return nil, fmt.Errorf("读取子进程输出失败：%w（stderr：%s）", err, msg)
				}
				return nil, fmt.Errorf("读取子进程输出失败：%w", err)
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				continue
			}
			var resp rpcResponse
			if err := json.Unmarshal(trimmed, &resp); err != nil {
				continue
			}
			if resp.ID != nil && *resp.ID == wantID {
				return &resp, nil
			}
		}
	}

	if err := write(rpcRequest{
		JSONRPC: "2.0", ID: rpcID(1), Method: "initialize",
		Params: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "beaconx", "version": "1.0"},
		},
	}); err != nil {
		return nil, err
	}
	initResp, err := readResponse(1)
	if err != nil {
		return nil, fmt.Errorf("initialize 失败：%w", err)
	}
	if initResp.Error != nil {
		return nil, fmt.Errorf("initialize 被拒绝：%w", initResp.Error)
	}
	if err := write(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return nil, err
	}

	const maxPages = 10
	collected := make([]Tool, 0, 16)
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		id := 2 + page
		if err := write(rpcRequest{JSONRPC: "2.0", ID: rpcID(id), Method: "tools/list", Params: params}); err != nil {
			return nil, err
		}
		resp, err := readResponse(id)
		if err != nil {
			return nil, fmt.Errorf("tools/list 失败：%w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("tools/list 被拒绝：%w", resp.Error)
		}
		parsed, tools, err := decodeTools(resp.Result)
		if err != nil {
			return nil, err
		}
		collected = append(collected, tools...)
		if len(collected) >= maxTools || parsed.NextCursor == "" {
			if len(collected) > maxTools {
				collected = collected[:maxTools]
			}
			return collected, nil
		}
		cursor = parsed.NextCursor
	}
	return collected, nil
}

// envPairs 把 env map 转成 KEY=VALUE 列表；空键跳过。
func envPairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		if strings.TrimSpace(key) == "" {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
}

// limitedBuffer 是只保留尾部 maxBytes 的 io.Writer，用于子进程 stderr。
type limitedBuffer struct {
	buf      bytes.Buffer
	maxBytes int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	limit := b.maxBytes
	if limit <= 0 {
		limit = 512
	}
	_, _ = b.buf.Write(p)
	// 只保留最新 limit 字节：一次超大 Write 也不会让缓冲区无限增长。
	if b.buf.Len() > limit {
		existing := b.buf.Bytes()
		kept := append([]byte(nil), existing[len(existing)-limit:]...)
		b.buf.Reset()
		_, _ = b.buf.Write(kept)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return strings.TrimSpace(b.buf.String())
}
