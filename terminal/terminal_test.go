package terminal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRunner(t *testing.T) *Runner {
	t.Helper()
	return &Runner{WorkDir: t.TempDir(), Users: []string{"u1"}}
}

func TestAllowed(t *testing.T) {
	r := &Runner{Users: []string{"u1"}}
	if !r.Allowed("u1") || r.Allowed("u2") || r.Allowed("") {
		t.Fatal("allowlist not applied")
	}
	if !(&Runner{Users: []string{"*"}}).Allowed("anyone") {
		t.Fatal("* should allow everyone")
	}
	var none *Runner
	if none.Allowed("u1") || (&Runner{}).Allowed("u1") {
		t.Fatal("no runner or no users should allow no one")
	}
}

func TestRunCommand(t *testing.T) {
	r := newRunner(t)
	dir, err := r.Dir("u1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), dir, "echo hello; echo oops >&2; exit 3", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.Contains(res.Output, "hello") || !strings.Contains(res.Output, "oops") {
		t.Fatalf("result = %+v", res)
	}
}

func TestDirIsPerChatAndPersists(t *testing.T) {
	r := newRunner(t)
	a, _ := r.Dir("u1", "c1")
	b, _ := r.Dir("u1", "c2")
	again, _ := r.Dir("u1", "c1")
	if a == b || a != again {
		t.Fatalf("dirs: %s %s %s", a, b, again)
	}
	if _, err := r.Run(context.Background(), a, "echo 42 > n.txt", 0); err != nil {
		t.Fatal(err)
	}
	res, _ := r.Run(context.Background(), again, "cat n.txt", 0)
	if strings.TrimSpace(res.Output) != "42" {
		t.Fatalf("file not kept: %q", res.Output)
	}
}

func TestServerEnvironmentIsNotPassedOn(t *testing.T) {
	t.Setenv("POLZA_API_KEY", "secret-key")
	r := newRunner(t)
	dir, _ := r.Dir("u1", "c1")
	res, err := r.Run(context.Background(), dir, "env", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "secret-key") {
		t.Fatal("server environment leaked into the command")
	}
	if !strings.Contains(res.Output, "HOME="+dir) {
		t.Fatalf("HOME not set to the working dir: %s", res.Output)
	}
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	r := newRunner(t)
	dir, _ := r.Dir("u1", "c1")
	started := time.Now()
	res, err := r.Run(context.Background(), dir, "sleep 30 & sleep 30", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("result = %+v", res)
	}
	if time.Since(started) > 10*time.Second {
		t.Fatal("timeout did not stop the command")
	}
}

func TestCancelStops(t *testing.T) {
	r := newRunner(t)
	dir, _ := r.Dir("u1", "c1")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := r.Run(ctx, dir, "sleep 30", 0)
	if err == nil {
		t.Fatal("want the context's error")
	}
}

func TestOutputIsCapped(t *testing.T) {
	r := newRunner(t)
	r.MaxOutput = 100
	dir, _ := r.Dir("u1", "c1")
	res, _ := r.Run(context.Background(), dir, "head -c 5000 /dev/zero | tr '\\0' x", 0)
	if len(res.Output) != 100 || !res.Truncated {
		t.Fatalf("len=%d truncated=%v", len(res.Output), res.Truncated)
	}
}

func TestRunScript(t *testing.T) {
	r := newRunner(t)
	dir, _ := r.Dir("u1", "c1")
	path, res, err := r.RunScript(context.Background(), dir, Bash, "echo from-script; pwd", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "from-script") || !strings.Contains(res.Output, dir) {
		t.Fatalf("output = %q", res.Output)
	}
	if filepath.Dir(path) != filepath.Join(dir, ".scripts") {
		t.Fatalf("script saved at %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if r.python() != "" {
		_, res, err := r.RunScript(context.Background(), dir, Python, "print(sum(range(10)))", 0)
		if err != nil || strings.TrimSpace(res.Output) != "45" {
			t.Fatalf("python: %q %v", res.Output, err)
		}
	}
	if _, _, err := r.RunScript(context.Background(), dir, "cobol", "x", 0); err == nil {
		t.Fatal("unknown language should fail")
	}
}

func TestTimeoutIsClamped(t *testing.T) {
	r := &Runner{Timeout: time.Second, MaxTimeout: 5 * time.Second}
	if got := r.clampTimeout(0); got != time.Second {
		t.Fatalf("default = %v", got)
	}
	if got := r.clampTimeout(time.Hour); got != 5*time.Second {
		t.Fatalf("max = %v", got)
	}
}
