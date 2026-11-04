//go:build linux

package terminal

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestHideServerSecrets(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may read any process's /proc entries; run as a normal user")
	}
	// Reading it has to work before, or the check below proves nothing.
	r := newRunner(t)
	dir, err := r.Dir("u1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	read := fmt.Sprintf("cat /proc/%d/environ", os.Getpid())
	if res, err := r.Run(context.Background(), dir, read, 0); err != nil || res.ExitCode != 0 {
		t.Fatalf("before: result = %+v, err = %v", res, err)
	}

	if err := HideServerSecrets(); err != nil {
		t.Fatal(err)
	}
	dumpable, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if errno != 0 || dumpable != 0 {
		t.Fatalf("dumpable = %d, errno = %v", dumpable, errno)
	}

	for _, path := range []string{"environ", "mem"} {
		cmd := fmt.Sprintf("cat /proc/%d/%s", os.Getpid(), path)
		res, err := r.Run(context.Background(), dir, cmd, 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode == 0 {
			t.Fatalf("%s readable after hiding: %+v", path, res)
		}
	}

	// Commands still work as before, their own /proc entries included.
	res, err := r.Run(context.Background(), dir, "cat /proc/self/environ | tr '\\0' '\\n' | grep ^HOME=", 0)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "HOME=") {
		t.Fatalf("command after hiding: result = %+v, err = %v", res, err)
	}
}
