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
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	// maxStdioOutputBytes 是 stdio 子进程 stdout 的读取上限：超过即终止发现，
	// 防止实现异常的服务器用无限输出拖垮客户端。
	maxStdioOutputBytes int64 = 1 << 20
)

// stdioDirEnv 是「允许执行哪些目录下的 stdio 命令」的配置键（逗号分隔绝对目录）。
const stdioDirEnv = "BEACONX_MCP_STDIO_DIR"

// stdioEnvAllowlist 是允许从父进程透传给 stdio 子进程的环境变量名。
//
// 默认只放行「运行一个普通 CLI 必需」的变量；其它变量（尤其是 *_KEY、
// *_TOKEN、AWS_*、代理凭据等）一律不继承，避免 MCP 子进程读走平台凭据。
// 端点自身配置的 env 会全部显式透传（那是运营者有意提供的，例如 MCP 自身的
// API Key）。
var stdioEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true,
	"TMPDIR": true, "TEMP": true, "TMP": true, "PWD": true,
}

// stdioSupported 报告当前平台是否支持 stdio 传输。
//
// Windows 上不支持：进程创建、管道与信号语义差异较大，最小实现不做适配；
// 声明为变量是为了让单测能在非 Windows 上覆盖「不支持」分支。
var stdioSupported = runtime.GOOS != "windows"

// StdioSupported 报告当前平台是否支持 MCP stdio（HTTP 层据此返回 400）。
func StdioSupported() bool { return stdioSupported }

// ErrStdioUnsupported 表示平台不支持 stdio 传输。
var ErrStdioUnsupported = errors.New("当前平台不支持 MCP stdio 传输")

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
	// StdioDirs 覆盖允许执行命令的目录列表；nil 表示读环境变量
	// BEACONX_MCP_STDIO_DIR（逗号分隔）。空列表 = 只接受精确白名单命令。
	StdioDirs []string
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

// stdioAllowed 判断本次是否允许 stdio，以及命令是否命中策略。
//
// 策略（从宽到严）：
//  1. 平台支持（Windows 直接拒绝）；
//  2. 显式开启 AllowStdio（默认 false）；
//  3. 命令非空，且满足下列之一：
//     a) 精确命中 StdioAllowlist；
//     b) 是绝对路径，且位于 BEACONX_MCP_STDIO_DIR / Options.StdioDirs 列出的目录内。
//     两者都不满足时拒绝；目录列表为空则只接受精确白名单。
func stdioAllowed(srv Server, opts Options) error {
	if !stdioSupported {
		return ErrStdioUnsupported
	}
	if !opts.AllowStdio {
		return ErrStdioDisabled
	}
	_, err := resolveStdioCommand(srv, opts)
	return err
}

// resolveStdioCommand 按白名单策略解析要执行的命令，返回可直接交给 exec 的路径。
func resolveStdioCommand(srv Server, opts Options) (string, error) {
	command := strings.TrimSpace(srv.Command)
	if command == "" {
		return "", errors.New("stdio 端点缺少 command")
	}
	for _, allowed := range opts.StdioAllowlist {
		if strings.TrimSpace(allowed) == command {
			return command, nil
		}
	}
	if !filepath.IsAbs(command) {
		return "", fmt.Errorf("%w：命令必须是绝对路径或精确命中白名单（%s）", ErrCommandNotAllowed, command)
	}
	dirs := stdioDirs(opts)
	if len(dirs) == 0 {
		return "", fmt.Errorf("%w：未配置 %s，仅允许精确白名单命令", ErrCommandNotAllowed, stdioDirEnv)
	}
	realCommand := command
	if resolved, err := filepath.EvalSymlinks(command); err == nil {
		realCommand = resolved
	}
	for _, dir := range dirs {
		realDir := dir
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			realDir = resolved
		}
		if withinDir(realDir, realCommand) {
			return command, nil
		}
	}
	return "", fmt.Errorf("%w：命令不在允许目录内", ErrCommandNotAllowed)
}

// stdioDirs 返回允许执行命令的目录列表：Options.StdioDirs 优先，否则读环境变量。
func stdioDirs(opts Options) []string {
	if opts.StdioDirs != nil {
		return opts.StdioDirs
	}
	raw := strings.TrimSpace(os.Getenv(stdioDirEnv))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || !filepath.IsAbs(trimmed) {
			continue
		}
		out = append(out, filepath.Clean(trimmed))
	}
	return out
}

// withinDir 报告 target 是否位于 dir 之下（两者都应是绝对路径）。
func withinDir(dir, target string) bool {
	cleanDir := filepath.Clean(dir)
	cleanTarget := filepath.Clean(target)
	if cleanTarget == cleanDir {
		return true
	}
	prefix := cleanDir
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(cleanTarget, prefix)
}

// sandboxEnv 构造 stdio 子进程的环境变量：父进程白名单 + 端点显式配置。
func sandboxEnv(configured map[string]string) []string {
	out := make([]string, 0, len(stdioEnvAllowlist)+len(configured))
	for _, kv := range os.Environ() {
		key, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if stdioEnvAllowlist[key] {
			out = append(out, kv)
		}
	}
	for key, value := range configured {
		name := strings.TrimSpace(key)
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
			continue
		}
		out = append(out, name+"="+value)
	}
	return out
}

// countingReader 统计已读取字节数，用于 stdio 输出上限。
type countingReader struct {
	reader io.Reader
	read   int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.read += int64(n)
	return n, err
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
// 沙箱约束：
//   - 平台不支持（Windows）→ ErrStdioUnsupported；
//   - 默认禁用，需显式开启；命令须命中精确白名单或位于允许目录；
//   - 子进程工作目录是独立临时目录（退出即删），不继承平台工作目录；
//   - 环境变量只透传白名单变量 + 端点显式配置，父进程密钥不外泄；
//   - stdout 读取上限 1MiB，超限终止；超时由 context 终止（子进程被 Kill）。
func discoverStdio(ctx context.Context, srv Server, opts Options) ([]Tool, error) {
	// 先过策略门（平台/开关/命令白名单），再解析可执行路径。
	if err := stdioAllowed(srv, opts); err != nil {
		return nil, err
	}
	command, err := resolveStdioCommand(srv, opts)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	// 独立工作目录：避免把平台 CWD（可能是仓库/配置目录）暴露给子进程。
	workDir, err := os.MkdirTemp("", "beaconx-mcp-stdio-*")
	if err != nil {
		return nil, fmt.Errorf("创建 stdio 沙箱目录失败：%w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	// CI/高并发下 fork/exec 偶发 EAGAIN（resource temporarily unavailable），
	// 这属于可重试的瞬时错误；重试时重建 Cmd（exec.Cmd 不可复用）。
	var (
		cmd    *exec.Cmd
		stdin  io.WriteCloser
		stdout io.ReadCloser
		stderr limitedBuffer
	)
	for attempt := 0; ; attempt++ {
		cmd = exec.CommandContext(ctx, command, srv.Args...)
		cmd.Dir = workDir
		cmd.Env = sandboxEnv(srv.Env)
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("创建子进程输入管道失败：%w", err)
		}
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("创建子进程输出管道失败：%w", err)
		}
		// stderr 只保留尾部少量字节用于诊断；同样不回显 env。
		stderr = limitedBuffer{}
		cmd.Stderr = &stderr
		// 独立进程组：超时可整组终止，防止子进程继续持有 stdout 管道。
		configureProcessGroup(cmd)
		if err = cmd.Start(); err == nil {
			break
		}
		if attempt >= 2 || ctx.Err() != nil {
			return nil, fmt.Errorf("启动 stdio MCP 失败：%w", err)
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	// 取消/超时时主动断开管道并杀掉进程组，保证 Read 立即返回而不是等到
	// 目标进程自然退出（否则一个 sleep 子进程就能拖住整个发现流程）。
	stopWatcher := make(chan struct{})
	defer close(stopWatcher)
	go func() {
		select {
		case <-ctx.Done():
			killProcessGroup(cmd)
			_ = stdin.Close()
			_ = stdout.Close()
		case <-stopWatcher:
		}
	}()
	defer func() {
		cancel()
		_ = stdin.Close()
		_ = stdout.Close()
		killProcessGroup(cmd)
		_ = cmd.Wait()
	}()

	counter := &countingReader{reader: io.LimitReader(stdout, maxStdioOutputBytes+1)}
	reader := bufio.NewReaderSize(counter, 64<<10)
	// readError 统一把「超时 / 输出超限 / EOF」翻译成可读错误。
	readError := func(err error) error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("stdio 发现超时（%s）", opts.timeout())
		}
		if counter.read > maxStdioOutputBytes {
			return fmt.Errorf("stdio 输出超过 %d 字节上限，已终止", maxStdioOutputBytes)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("读取子进程输出失败：%w（stderr：%s）", err, msg)
		}
		return fmt.Errorf("读取子进程输出失败：%w", err)
	}
	write := func(body rpcRequest) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求失败：%w", err)
		}
		raw = append(raw, '\n')
		if _, err := stdin.Write(raw); err != nil {
			if ctx.Err() != nil {
				return readError(err)
			}
			// 子进程可能在收到 initialize 前就退出（broken pipe）。此时要等它
			// 收尾并把 stderr 带进错误信息，否则「环境变量/工作目录」等诊断
			// 信息会丢失（CI 与本地时序不同，最容易暴露）。
			_ = stdin.Close()
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmt.Errorf("写入子进程失败：%w（stderr：%s）", err, msg)
			}
			return fmt.Errorf("写入子进程失败：%w", err)
		}
		return nil
	}
	// readResponse 读取直到遇到目标 id 的 JSON-RPC 响应；非 JSON 行（日志）跳过。
	readResponse := func(wantID int) (*rpcResponse, error) {
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return nil, readError(err)
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
// 子进程 stdout/stderr 由 os/exec 的独立 goroutine 写入，而超时/取消路径会并发
// 读取 String()，因此必须加锁。
type limitedBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	maxBytes int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}
