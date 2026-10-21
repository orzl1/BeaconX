//go:build !windows

package mcp

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup 让 stdio 子进程成为独立进程组的组长。
//
// 这样超时/取消时可以按进程组整体 Kill，把 MCP 服务器自己再 fork 出来的子进程
// 一并终止，避免它们继续持有 stdout 管道导致读取端阻塞。
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 杀死整个进程组（失败时静默，调用方通常已在错误路径上）。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// 进程组可能已消失或权限不足：退回单进程 Kill。
		_ = cmd.Process.Kill()
	}
}
