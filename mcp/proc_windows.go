//go:build windows

package mcp

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a console server (uvx, node, python) from opening a
// window of its own.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
