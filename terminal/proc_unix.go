//go:build unix

package terminal

import (
	"os/exec"
	"strings"
	"syscall"
)

// setProcessGroup puts the command in a process group of its own, so the
// time limit and Stop reach everything it started.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

func setRawCommandLine(cmd *exec.Cmd, line string) {}

func decodeOutput(data []byte) string {
	return strings.ToValidUTF8(string(data), "�")
}
