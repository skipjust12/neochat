// Package terminal runs shell commands and scripts on the server host for
// the model's run_command and run_script tools (server/terminal.go).
//
// Everything runs as the server's own OS user, in a working directory of
// its own per chat, with no stdin, a time limit, and a minimal
// environment: the server's environment (API keys, database passwords) is
// not passed on. That is not a sandbox: a command can do whatever the
// server's user can, read what it can read and reach what it can reach.
// Turn it on only for people you would hand a shell on this machine, and
// for isolation run the server in a container or as a user of its own.
package terminal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Defaults for a zero Runner.
const (
	DefaultTimeout   = 60 * time.Second
	MaxTimeout       = 10 * time.Minute
	DefaultMaxOutput = 1 << 20 // bytes kept of a command's output
)

// Runner runs commands on this machine.
type Runner struct {
	// WorkDir holds one working directory per chat (see Dir); empty: the
	// system temp directory's neochat-terminal.
	WorkDir string
	// Shell runs run_command and bash scripts; empty: bash, or sh when
	// there is no bash.
	Shell string
	// Python runs Python scripts; empty: python3, or python.
	Python string
	// Timeout is the default and MaxTimeout the longest time one command
	// may run; zero: DefaultTimeout and terminal.MaxTimeout.
	Timeout    time.Duration
	MaxTimeout time.Duration
	// MaxOutput is how many bytes of output are kept; zero:
	// DefaultMaxOutput.
	MaxOutput int
	// Users may use the terminal: user ids, or "*" for everyone. Empty:
	// no one.
	Users []string

	once        sync.Once
	description string
}

// Allowed reports whether userID may use the terminal.
func (r *Runner) Allowed(userID string) bool {
	if r == nil {
		return false
	}
	for _, u := range r.Users {
		if u == "*" || (u != "" && u == userID) {
			return true
		}
	}
	return false
}

// Result is what one command did.
type Result struct {
	// ExitCode is the process's exit status; -1 when it was killed (time
	// limit, Stop) or never started.
	ExitCode int
	// Output is stdout and stderr together, in the order written.
	Output    string
	Truncated bool // output went past MaxOutput; the rest was dropped
	TimedOut  bool
	Duration  time.Duration
}

// Error is a command that could not run at all (no such interpreter, no
// working directory); its Message is fit to show.
type Error struct{ Message string }

func (e *Error) Error() string { return "terminal: " + e.Message }

// Dir is the working directory for one chat, created on first use. The
// same user and chat always get the same directory, so files a script
// wrote are there for the next command; chat is empty for one-off runs
// (private chats), which share one directory per user.
func (r *Runner) Dir(userID, chat string) (string, error) {
	base := r.WorkDir
	if base == "" {
		base = filepath.Join(os.TempDir(), "neochat-terminal")
	}
	sum := sha256.Sum256([]byte(userID + "\x00" + chat))
	dir := filepath.Join(base, hex.EncodeToString(sum[:])[:24])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", &Error{Message: "can't create the working directory: " + err.Error()}
	}
	return dir, nil
}

// Run runs a shell command in dir.
func (r *Runner) Run(ctx context.Context, dir, command string, timeout time.Duration) (Result, error) {
	shell := r.shell()
	if shell == "" {
		return Result{ExitCode: -1}, &Error{Message: "no shell (bash or sh) on this machine"}
	}
	return r.exec(ctx, dir, timeout, shell, "-c", command)
}

// Script languages for RunScript.
const (
	Python = "python"
	Bash   = "bash"
)

// RunScript saves code as a file in dir's .scripts and runs it with the
// language's interpreter, in dir. It returns the script's path.
func (r *Runner) RunScript(ctx context.Context, dir, language, code string, timeout time.Duration) (string, Result, error) {
	var interpreter, ext string
	switch language {
	case Python:
		interpreter, ext = r.python(), ".py"
		if interpreter == "" {
			return "", Result{ExitCode: -1}, &Error{Message: "no Python (python3) on this machine"}
		}
	case Bash:
		interpreter, ext = r.shell(), ".sh"
		if interpreter == "" {
			return "", Result{ExitCode: -1}, &Error{Message: "no shell (bash or sh) on this machine"}
		}
	default:
		return "", Result{ExitCode: -1}, &Error{Message: fmt.Sprintf("unknown language %q (use python or bash)", language)}
	}
	scripts := filepath.Join(dir, ".scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		return "", Result{ExitCode: -1}, &Error{Message: "can't save the script: " + err.Error()}
	}
	path := filepath.Join(scripts, fmt.Sprintf("script-%s%s", time.Now().UTC().Format("20060102-150405.000000"), ext))
	if err := os.WriteFile(path, []byte(code), 0o600); err != nil {
		return "", Result{ExitCode: -1}, &Error{Message: "can't save the script: " + err.Error()}
	}
	res, err := r.exec(ctx, dir, timeout, interpreter, path)
	return path, res, err
}

func (r *Runner) exec(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (Result, error) {
	timeout = r.clampTimeout(timeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Dir = dir
	cmd.Env = r.env(dir)
	cmd.Stdin = nil // /dev/null: nothing waits for input
	out := &capped{max: r.maxOutput()}
	cmd.Stdout, cmd.Stderr = out, out
	setProcessGroup(cmd)
	// Kill the whole process group, not just the shell, and stop waiting
	// for pipes a background child keeps open.
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	started := time.Now()
	err := cmd.Run()
	res := Result{ExitCode: 0, Output: out.String(), Truncated: out.dropped, Duration: time.Since(started)}
	if err == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.ExitCode, res.TimedOut = -1, true
		return res, nil
	case ctx.Err() != nil:
		res.ExitCode = -1
		return res, ctx.Err()
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	case errors.Is(err, exec.ErrWaitDelay):
		// The command finished; a background child kept its output open.
		if cmd.ProcessState != nil {
			res.ExitCode = cmd.ProcessState.ExitCode()
		}
		return res, nil
	}
	res.ExitCode = -1
	return res, &Error{Message: "can't run it: " + err.Error()}
}

func (r *Runner) clampTimeout(t time.Duration) time.Duration {
	def, most := r.Timeout, r.MaxTimeout
	if def <= 0 {
		def = DefaultTimeout
	}
	if most <= 0 {
		most = MaxTimeout
	}
	if t <= 0 {
		t = def
	}
	return min(t, most)
}

// TimeoutLimits are the default and longest time limit, for the model's
// instructions.
func (r *Runner) TimeoutLimits() (def, most time.Duration) {
	return r.clampTimeout(0), r.clampTimeout(1 << 62)
}

func (r *Runner) maxOutput() int {
	if r.MaxOutput > 0 {
		return r.MaxOutput
	}
	return DefaultMaxOutput
}

func (r *Runner) shell() string {
	if r.Shell != "" {
		return r.Shell
	}
	return firstInPath("bash", "sh")
}

func (r *Runner) python() string {
	if r.Python != "" {
		return r.Python
	}
	return firstInPath("python3", "python")
}

func firstInPath(names ...string) string {
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// env is the environment commands get: enough to work, nothing of the
// server's own (keys, passwords).
func (r *Runner) env(dir string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=dumb",
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
		"PAGER=cat",
		"GIT_PAGER=cat",
		"NO_COLOR=1",
	}
	// The way out to the internet, where the host has one.
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// Describe is one line about this machine for the model: system,
// architecture, shell and Python version. Worked out once.
func (r *Runner) Describe() string {
	r.once.Do(func() {
		parts := []string{runtime.GOOS + "/" + runtime.GOARCH}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if runtime.GOOS == "linux" {
			if data, err := os.ReadFile("/etc/os-release"); err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
						parts = append(parts, strings.Trim(v, `"`))
					}
				}
			}
		}
		if sh := r.shell(); sh != "" {
			parts = append(parts, "shell "+filepath.Base(sh))
		}
		if py := r.python(); py != "" {
			if out, err := exec.CommandContext(ctx, py, "--version").CombinedOutput(); err == nil {
				parts = append(parts, strings.TrimSpace(string(out)))
			}
		} else {
			parts = append(parts, "no Python")
		}
		r.description = strings.Join(parts, ", ")
	})
	return r.description
}

// capped keeps the first max bytes written to it.
type capped struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	max     int
	dropped bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room < len(p) {
		c.dropped = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.ToValidUTF8(c.buf.String(), "�")
}
