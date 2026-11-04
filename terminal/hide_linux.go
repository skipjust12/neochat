//go:build linux

package terminal

import "syscall"

// HideServerSecrets makes this process non-dumpable. Commands run as the
// server's own OS user, and without this they could read its environment
// (API keys, database passwords) from /proc/<pid>/environ, or its memory
// from /proc/<pid>/mem, however clean the environment they were started
// with. Commands themselves are unaffected: exec makes a new process
// dumpable again. A call before anything else in main covers the whole
// process.
func HideServerSecrets() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
