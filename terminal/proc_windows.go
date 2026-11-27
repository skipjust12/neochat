//go:build windows

package terminal

import (
	"os/exec"
	"strconv"
	"syscall"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

const createNoWindow = 0x08000000

// setProcessGroup starts the command without a console window of its own
// (the desktop app runs in the background).
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow | syscall.CREATE_NEW_PROCESS_GROUP
	cmd.SysProcAttr.HideWindow = true
}

// killProcessGroup ends the command and everything it started: Windows
// has no process groups to signal, taskkill /T walks the tree.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

func setRawCommandLine(cmd *exec.Cmd, line string) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = line
}

// decodeOutput reads what a command printed: UTF-8 when it is (the
// shells are switched to it), else the console's OEM code page, which
// most Windows programs still write (CP866 on Russian systems).
func decodeOutput(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	if out, err := charmap.CodePage866.NewDecoder().Bytes(data); err == nil {
		return string(out)
	}
	return string(data)
}
