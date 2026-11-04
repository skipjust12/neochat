package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"neochat/mcp"
	"neochat/terminal"
)

// Executor runs Requests on this computer: the desktop app's side.
type Executor struct {
	Runner *terminal.Runner
	// MCP runs the app's MCP servers; nil: none.
	MCP *mcp.Manager
	// WorkDir holds one working directory per chat.
	WorkDir string
}

// Limits on file requests.
const (
	ReadWindow    = 50_000   // characters one read_file returns
	MaxReadBytes  = 20 << 20 // a file read_file will open
	MaxWriteBytes = 20 << 20
	MaxFetchBytes = 25 << 20
	MaxListed     = 500
	MaxMCPOutput  = 50_000
)

// Dir is a chat's working directory, created on first use.
func (e *Executor) Dir(chat string) (string, error) {
	sum := sha256.Sum256([]byte(chat))
	dir := filepath.Join(e.WorkDir, hex.EncodeToString(sum[:])[:16])
	if chat == "" {
		dir = filepath.Join(e.WorkDir, "scratch")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("can't create the working directory: %v", err)
	}
	return dir, nil
}

// Hello describes this computer.
func (e *Executor) Hello(version string) Hello {
	name, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	return Hello{
		Version: version, Name: name, OS: osName(), Description: e.Runner.Describe(),
		User: user, Home: home, WorkDir: e.WorkDir,
		Shells: e.Runner.Shells(), Languages: e.Runner.Languages(), MCP: e.MCPServers(),
	}
}

// MCPServers lists the MCP servers and their tools.
func (e *Executor) MCPServers() []Server {
	if e.MCP == nil {
		return nil
	}
	var out []Server
	for _, s := range e.MCP.Servers() {
		server := Server{Name: s.Name, Error: s.Error}
		for _, t := range s.Tools {
			server.Tools = append(server.Tools, Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
		out = append(out, server)
	}
	return out
}

// Run runs one request.
func (e *Executor) Run(ctx context.Context, req Request) Result {
	res := e.run(ctx, req)
	res.ID = req.ID
	return res
}

func (e *Executor) run(ctx context.Context, req Request) Result {
	dir, err := e.Dir(req.Chat)
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1}
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	switch req.Op {
	case OpRunCommand:
		res, err := e.Runner.RunShell(ctx, dir, req.Shell, req.Command, timeout)
		return commandResult(res, err)
	case OpRunScript:
		_, res, err := e.Runner.RunScript(ctx, dir, req.Language, req.Code, timeout)
		return commandResult(res, err)
	case OpReadFile:
		return e.readFile(dir, req)
	case OpWriteFile:
		return e.writeFile(dir, req)
	case OpListDir:
		return e.listDir(dir, req)
	case OpFetchFile:
		return e.fetchFile(dir, req)
	case OpMCPCall:
		return e.mcpCall(ctx, req)
	}
	return Result{Error: fmt.Sprintf("unknown operation %q", req.Op), ExitCode: -1}
}

func commandResult(res terminal.Result, err error) Result {
	out := Result{ExitCode: res.ExitCode, Output: res.Output, Truncated: res.Truncated, TimedOut: res.TimedOut, DurationMS: res.Duration.Milliseconds()}
	var termErr *terminal.Error
	switch {
	case errors.As(err, &termErr):
		out.Error = termErr.Message
	case errors.Is(err, context.Canceled):
		out.Error = "stopped"
	case err != nil:
		out.Error = err.Error()
	}
	return out
}

// resolve makes a path absolute against dir; ~ is the home directory.
func resolve(dir, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is empty")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[1:])
	}
	path = os.ExpandEnv(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path), nil
}

func (e *Executor) readFile(dir string, req Request) Result {
	path, err := resolve(dir, req.Path)
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1}
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return Result{Error: "no such file: " + path, ExitCode: -1, Path: path}
	case info.IsDir():
		return Result{Error: path + " is a folder: use list_dir", ExitCode: -1, Path: path}
	case info.Size() > MaxReadBytes:
		return Result{Error: fmt.Sprintf("%s is %d MB; read_file opens files up to %d MB (use a command or a script for bigger ones)", path, info.Size()>>20, MaxReadBytes>>20), ExitCode: -1, Path: path}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1, Path: path}
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return Result{Error: path + " is not a text file: use a script to read it", ExitCode: -1, Path: path, Size: info.Size()}
	}
	runes := []rune(strings.TrimPrefix(string(data), "\ufeff"))
	limit := req.Limit
	if limit <= 0 || limit > ReadWindow {
		limit = ReadWindow
	}
	from := max(0, min(req.Offset, len(runes)))
	to := min(len(runes), from+limit)
	return Result{Output: string(runes[from:to]), Path: path, Size: info.Size(), Chars: len(runes), Truncated: to < len(runes), ExitCode: 0}
}

func (e *Executor) writeFile(dir string, req Request) Result {
	path, err := resolve(dir, req.Path)
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1}
	}
	if len(req.Content) > MaxWriteBytes {
		return Result{Error: "the content is too large", ExitCode: -1, Path: path}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{Error: err.Error(), ExitCode: -1, Path: path}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if req.Append {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1, Path: path}
	}
	_, err = f.WriteString(req.Content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1, Path: path}
	}
	info, _ := os.Stat(path)
	size := int64(len(req.Content))
	if info != nil {
		size = info.Size()
	}
	return Result{Path: path, Size: size, ExitCode: 0}
}

func (e *Executor) listDir(dir string, req Request) Result {
	path := dir
	if strings.TrimSpace(req.Path) != "" {
		var err error
		if path, err = resolve(dir, req.Path); err != nil {
			return Result{Error: err.Error(), ExitCode: -1}
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1, Path: path}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	var b strings.Builder
	for i, entry := range entries {
		if i == MaxListed {
			fmt.Fprintf(&b, "… and %d more\n", len(entries)-MaxListed)
			break
		}
		if entry.IsDir() {
			fmt.Fprintf(&b, "%s%c\n", entry.Name(), filepath.Separator)
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&fs.ModeType != 0 {
			fmt.Fprintf(&b, "%s\n", entry.Name())
			continue
		}
		fmt.Fprintf(&b, "%s  %s  %s\n", entry.Name(), sizeText(info.Size()), info.ModTime().Format("2006-01-02 15:04"))
	}
	if len(entries) == 0 {
		b.WriteString("(empty)")
	}
	return Result{Output: strings.TrimRight(b.String(), "\n"), Path: path, ExitCode: 0, Size: int64(len(entries))}
}

func (e *Executor) fetchFile(dir string, req Request) Result {
	path, err := resolve(dir, req.Path)
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1}
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return Result{Error: "no such file: " + path, ExitCode: -1}
	case info.IsDir():
		return Result{Error: path + " is a folder: pack it into an archive first", ExitCode: -1}
	case info.Size() == 0:
		return Result{Error: path + " is empty", ExitCode: -1}
	case info.Size() > MaxFetchBytes:
		return Result{Error: fmt.Sprintf("%s is %s; files are limited to %s", path, sizeText(info.Size()), sizeText(MaxFetchBytes)), ExitCode: -1}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{Error: err.Error(), ExitCode: -1}
	}
	return Result{Data: data, Name: filepath.Base(path), Path: path, Size: int64(len(data)), ExitCode: 0}
}

func (e *Executor) mcpCall(ctx context.Context, req Request) Result {
	if e.MCP == nil {
		return Result{Error: "no MCP servers are set up in the app", ExitCode: -1}
	}
	started := time.Now()
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := e.MCP.Call(callCtx, req.Server, req.Tool, req.Arguments)
	out := Result{DurationMS: time.Since(started).Milliseconds()}
	if err != nil {
		out.Error, out.ExitCode = err.Error(), -1
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			out.Error, out.TimedOut = "the tool took too long", true
		}
		return out
	}
	text := res.Text()
	if runes := []rune(text); len(runes) > MaxMCPOutput {
		text, out.Truncated = string(runes[:MaxMCPOutput]), true
	}
	out.Output, out.IsError = text, res.IsError
	if res.IsError {
		out.ExitCode = 1
	}
	return out
}

func sizeText(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", (n+512)>>10)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}
