//go:build windows

package mcp

import "os/exec"

// configureProcessGroup 在 Windows 上没有进程组语义，保留空实现（应为空构造）。
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup 在 Windows 上只终止单个进程。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
