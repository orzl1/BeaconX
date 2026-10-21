package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/policy"
)

// ShellExec 在受限超时内执行一条 shell 命令（高风险工具，默认需要审批）。
type ShellExec struct{}

// NewShellExec 创建命令执行工具。
func NewShellExec() *ShellExec { return &ShellExec{} }

type shellExecArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	WorkDir        string `json:"work_dir,omitempty"`
}

// Definition 实现 Tool。
func (*ShellExec) Definition() Definition {
	return Definition{
		Name:        "shell_exec",
		Description: "执行一条 shell 命令并返回合并后的输出（最多 32KB）。高风险，通常需要人工审批。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的命令"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数（1-120）"},
    "work_dir": {"type": "string", "description": "工作目录，可选"}
  },
  "required": ["command"]
}`),
		Risk:  policy.RiskHigh,
		Roles: []string{"worker"},
	}
}

// Execute 实现 Tool。
func (*ShellExec) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args shellExecArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return Result{}, fmt.Errorf("参数解析失败: %w", err)
	}
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return Result{}, fmt.Errorf("command 不能为空")
	}
	timeout := time.Duration(args.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(runCtx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(runCtx, "/bin/sh", "-c", command)
	}
	if args.WorkDir != "" {
		cmd.Dir = args.WorkDir
	}
	out := &limitedBuffer{max: maxToolOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return Result{Output: out.String(), Meta: map[string]any{"timeout_seconds": int(timeout.Seconds())}},
			fmt.Errorf("命令执行超时（%s）", timeout)
	}
	if err != nil {
		return Result{Output: out.String(), Meta: map[string]any{"error": err.Error()}},
			fmt.Errorf("命令执行失败: %w；输出：%s", err, truncateOutput(out.String()))
	}
	return Result{Output: out.String(), Meta: map[string]any{"ok": true}}, nil
}

// limitedBuffer 只保留前 max 字节，避免失控输出占满内存。
type limitedBuffer struct {
	buf []byte
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if remaining := b.max - len(b.buf); remaining > 0 {
		if len(p) > remaining {
			b.buf = append(b.buf, p[:remaining]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return string(b.buf) }

func truncateOutput(s string) string {
	const n = 512
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
