package device

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"neochat/terminal"
)

func newExecutor(t *testing.T) *Executor {
	work := t.TempDir()
	return &Executor{Runner: &terminal.Runner{WorkDir: work, InheritEnv: true}, WorkDir: work}
}

func TestExecutorFiles(t *testing.T) {
	e := newExecutor(t)
	ctx := context.Background()
	if res := e.Run(ctx, Request{ID: "1", Op: OpWriteFile, Chat: "c", Path: "a/b.txt", Content: "привет\nworld"}); res.Error != "" || res.ID != "1" {
		t.Fatalf("write = %+v", res)
	}
	res := e.Run(ctx, Request{Op: OpReadFile, Chat: "c", Path: "a/b.txt", Offset: 2})
	if res.Error != "" || res.Output != "ивет\nworld" || res.Chars != 12 {
		t.Fatalf("read = %+v", res)
	}
	if res := e.Run(ctx, Request{Op: OpWriteFile, Chat: "c", Path: "a/b.txt", Content: "!", Append: true}); res.Error != "" {
		t.Fatal(res.Error)
	}
	list := e.Run(ctx, Request{Op: OpListDir, Chat: "c"})
	if !strings.Contains(list.Output, "a"+string(filepath.Separator)) {
		t.Fatalf("list = %+v", list)
	}
	fetched := e.Run(ctx, Request{Op: OpFetchFile, Chat: "c", Path: "a/b.txt"})
	if string(fetched.Data) != "привет\nworld!" || fetched.Name != "b.txt" {
		t.Fatalf("fetch = %+v", fetched)
	}
	if res := e.Run(ctx, Request{Op: OpReadFile, Chat: "c", Path: "missing.txt"}); res.Error == "" {
		t.Fatal("missing file should fail")
	}
	bin := filepath.Join(t.TempDir(), "x.bin")
	_ = os.WriteFile(bin, []byte{0, 1, 2}, 0o600)
	if res := e.Run(ctx, Request{Op: OpReadFile, Path: bin}); !strings.Contains(res.Error, "not a text file") {
		t.Fatalf("binary = %+v", res)
	}
}

func TestExecutorCommands(t *testing.T) {
	e := newExecutor(t)
	res := e.Run(context.Background(), Request{Op: OpRunCommand, Chat: "c", Command: "echo $((6*7))"})
	if res.Error != "" || strings.TrimSpace(res.Output) != "42" || res.ExitCode != 0 {
		t.Fatalf("command = %+v", res)
	}
	res = e.Run(context.Background(), Request{Op: OpRunCommand, Shell: "cmd", Command: "dir"})
	if res.Error == "" {
		t.Fatal("cmd on Linux should say there is none")
	}
	res = e.Run(context.Background(), Request{Op: OpMCPCall, Server: "x", Tool: "y"})
	if res.Error == "" {
		t.Fatal("MCP without servers should fail")
	}
}

func TestConnectURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://chat.example.com": "wss://chat.example.com/device/connect",
		"http://localhost:8080/":   "ws://localhost:8080/device/connect",
		"https://example.com/neo/": "wss://example.com/neo/device/connect",
	} {
		if got, err := ConnectURL(in); err != nil || got != want {
			t.Errorf("ConnectURL(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ConnectURL("ftp://x"); err == nil {
		t.Error("ftp should fail")
	}
}
