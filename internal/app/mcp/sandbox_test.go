package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stdio 沙箱测试：全部使用 /bin/sh 脚本作为「MCP 服务器」，只验证隔离与限制，
// 不依赖任何真实 MCP 实现。Windows 不支持 stdio，相关用例整体跳过。

// skipOnWindows 跳过 stdio 用例（Windows 实现直接拒绝 stdio）。
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 MCP stdio")
	}
}

// writeSandboxScript 写一个可执行的 sh 脚本并返回绝对路径。
func writeSandboxScript(t *testing.T, body string) string {
	t.Helper()
	skipOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "server.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("写入测试脚本失败: %v", err)
	}
	return path
}

// sandboxOptions 构造「只允许执行该脚本」的选项。
func sandboxOptions(script string, timeout time.Duration) Options {
	return Options{AllowStdio: true, StdioAllowlist: []string{script}, Timeout: timeout}
}

// TestSandboxEnvWhitelist 验证父进程敏感环境变量不会被透传，而端点显式配置的
// 环境变量会透传。
func TestSandboxEnvWhitelist(t *testing.T) {
	skipOnWindows(t)
	script := writeSandboxScript(t, `printf 'MCP_TOKEN=%s B6_SECRET=%s\n' "$MCP_TOKEN" "$B6_SECRET" >&2
exit 1
`)
	t.Setenv("B6_SECRET", "leak-me-please")
	_, err := Discover(context.Background(), Server{
		Name: "sandbox-env", Transport: TransportStdio, Command: script,
		Env: map[string]string{"MCP_TOKEN": "configured-ok"},
	}, sandboxOptions(script, 5*time.Second))
	if err == nil {
		t.Fatal("期望子进程失败（脚本 exit 1）")
	}
	text := err.Error()
	if strings.Contains(text, "leak-me-please") {
		t.Fatalf("父进程敏感环境变量被透传: %s", text)
	}
	if !strings.Contains(text, "configured-ok") {
		t.Fatalf("端点显式配置的环境变量没有透传: %s", text)
	}
}

// TestSandboxWorkDir 验证子进程工作目录是独立临时目录，不是平台 CWD。
func TestSandboxWorkDir(t *testing.T) {
	skipOnWindows(t)
	script := writeSandboxScript(t, `pwd >&2
exit 1
`)
	_, err := Discover(context.Background(), Server{
		Name: "sandbox-cwd", Transport: TransportStdio, Command: script,
	}, sandboxOptions(script, 5*time.Second))
	if err == nil {
		t.Fatal("期望脚本退出错误")
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		t.Fatalf("读取当前目录失败: %v", cwdErr)
	}
	text := err.Error()
	if strings.Contains(text, cwd) {
		t.Fatalf("子进程继承了平台工作目录 %q: %s", cwd, text)
	}
	if !strings.Contains(text, os.TempDir()) && !strings.Contains(text, "beaconx-mcp-stdio") {
		t.Fatalf("子进程工作目录不是沙箱临时目录: %s", text)
	}
}

// TestSandboxTimeout 验证超时后子进程被终止、返回超时错误。
func TestSandboxTimeout(t *testing.T) {
	skipOnWindows(t)
	script := writeSandboxScript(t, `sleep 30
`)
	start := time.Now()
	_, err := Discover(context.Background(), Server{
		Name: "sandbox-timeout", Transport: TransportStdio, Command: script,
	}, sandboxOptions(script, 200*time.Millisecond))
	if err == nil {
		t.Fatal("期望超时错误")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误信息未体现超时: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时未及时终止，耗时 %s", elapsed)
	}
}

// TestSandboxOutputCap 验证 stdout 超限会被终止（不会无限读取）。
func TestSandboxOutputCap(t *testing.T) {
	skipOnWindows(t)
	script := writeSandboxScript(t, `while true; do
  printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n'
done
`)
	start := time.Now()
	_, err := Discover(context.Background(), Server{
		Name: "sandbox-output", Transport: TransportStdio, Command: script,
	}, sandboxOptions(script, 5*time.Second))
	if err == nil {
		t.Fatal("期望输出超限错误")
	}
	if !strings.Contains(err.Error(), "输出超过") {
		t.Fatalf("错误信息未体现输出上限: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("输出超限未及时终止，耗时 %s", elapsed)
	}
}

// TestSandboxCommandPolicy 验证命令白名单策略。
func TestSandboxCommandPolicy(t *testing.T) {
	skipOnWindows(t)
	ctx := context.Background()
	// 1) 相对路径且不在精确白名单 → 拒绝。
	if _, err := Discover(ctx, Server{Name: "rel", Transport: TransportStdio, Command: "echo"}, Options{AllowStdio: true}); !errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("相对命令期望 ErrCommandNotAllowed，实际: %v", err)
	}
	// 2) 绝对路径但未配置目录白名单 → 拒绝。
	script := writeSandboxScript(t, "exit 1\n")
	if _, err := Discover(ctx, Server{Name: "abs", Transport: TransportStdio, Command: script}, Options{AllowStdio: true}); !errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("未配置目录时期望 ErrCommandNotAllowed，实际: %v", err)
	}
	// 3) 目录白名单命中 → 放行到执行阶段（脚本 exit 1 会得到启动/通信错误，
	//    而不是策略错误）。
	_, err := Discover(ctx, Server{Name: "dir-ok", Transport: TransportStdio, Command: script}, Options{
		AllowStdio: true,
		StdioDirs:  []string{filepath.Dir(script)},
		Timeout:    5 * time.Second,
	})
	if errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("目录白名单命中却被拒绝: %v", err)
	}
	// 4) 目录白名单不命中 → 拒绝。
	otherDir := t.TempDir()
	if _, err := Discover(ctx, Server{Name: "dir-no", Transport: TransportStdio, Command: script}, Options{
		AllowStdio: true,
		StdioDirs:  []string{otherDir},
	}); !errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("目录白名单不命中期望 ErrCommandNotAllowed，实际: %v", err)
	}
}

// TestSandboxUnsupportedPlatform 验证非 Linux/macOS 平台直接拒绝。
func TestSandboxUnsupportedPlatform(t *testing.T) {
	original := stdioSupported
	stdioSupported = false
	defer func() { stdioSupported = original }()
	if StdioSupported() {
		t.Fatal("StdioSupported() 应为 false")
	}
	_, err := Discover(context.Background(), Server{Name: "win", Transport: TransportStdio, Command: "/bin/echo"}, Options{AllowStdio: true})
	if !errors.Is(err, ErrStdioUnsupported) {
		t.Fatalf("期望 ErrStdioUnsupported，实际: %v", err)
	}
}

// TestSandboxStdioDisabledDefault 验证默认仍禁用 stdio（AllowStdio=false）。
func TestSandboxStdioDisabledDefault(t *testing.T) {
	skipOnWindows(t)
	script := writeSandboxScript(t, "exit 1\n")
	_, err := Discover(context.Background(), Server{Name: "off", Transport: TransportStdio, Command: script}, Options{})
	if !errors.Is(err, ErrStdioDisabled) {
		t.Fatalf("期望 ErrStdioDisabled，实际: %v", err)
	}
}
