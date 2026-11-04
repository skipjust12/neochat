// Command neochat-desktop is NeoChat on your computer: it opens the chat in
// a window of its own and, while it runs, lets the models work on this
// computer -- commands, scripts, files and MCP servers (Blender and
// others) -- when "Execute commands on" in the composer's + menu is set to
// Computer. Close the window and the access ends.
//
// It connects out to the NeoChat server (no port is opened here) with
// your API key, and runs what the server sends (package device). The
// first run asks for the server's address and the key and keeps them in
// the config folder (%AppData%\NeoChat on Windows), next to mcp.json,
// the MCP servers to run, in Claude Desktop's format:
//
//	{"mcpServers": {"blender": {"command": "uvx", "args": ["blender-mcp"]}}}
//
// Build for Windows: GOOS=windows GOARCH=amd64 go build -o NeoChat.exe ./cmd/neochat-desktop
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"neochat/device"
	"neochat/mcp"
	"neochat/terminal"
)

const version = "0.1.0"

type config struct {
	Server string `json:"server"`
	Key    string `json:"key"`
	// WorkDir holds the per-chat working directories; default:
	// ~/NeoChat/work.
	WorkDir string `json:"work_dir,omitempty"`
	// TimeoutSeconds and MaxTimeoutSeconds are one command's default and
	// longest time limit (default 120 s and 10 min).
	TimeoutSeconds    int `json:"timeout_seconds,omitempty"`
	MaxTimeoutSeconds int `json:"max_timeout_seconds,omitempty"`
}

func main() {
	dirFlag := flag.String("config", "", "config folder (default: the user config folder's NeoChat)")
	serverFlag := flag.String("server", "", "NeoChat server address, e.g. https://chat.example.com (saved)")
	keyFlag := flag.String("key", "", "your NeoChat API key, nc_... (saved)")
	noWindow := flag.Bool("no-window", false, "don't open the chat window: only keep the computer connected")
	flag.Parse()

	dir := *dirFlag
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			fatal(err)
		}
		dir = filepath.Join(base, "NeoChat")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "neochat.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
		defer logFile.Close()
	}

	cfg, err := loadConfig(dir, *serverFlag, *keyFlag)
	if err != nil {
		fatal(err)
	}
	mcpPath := filepath.Join(dir, "mcp.json")
	if _, err := os.Stat(mcpPath); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(mcpPath, []byte("{\n  \"mcpServers\": {}\n}\n"), 0o600)
	}
	mcpConfig, err := mcp.LoadConfig(mcpPath)
	if err != nil {
		log.Printf("mcp.json: %v (no MCP servers this time)", err)
	}

	workDir := cfg.WorkDir
	if workDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fatal(err)
		}
		workDir = filepath.Join(home, "NeoChat", "work")
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		fatal(err)
	}
	runner := &terminal.Runner{
		WorkDir: workDir, InheritEnv: true, Users: []string{"*"},
		Timeout:    seconds(cfg.TimeoutSeconds, 120),
		MaxTimeout: seconds(cfg.MaxTimeoutSeconds, 600),
	}
	manager := mcp.NewManager(mcpConfig)
	defer manager.Close()
	client := &device.Client{
		Server: cfg.Server, Key: cfg.Key, Version: version,
		Executor: &device.Executor{Runner: runner, MCP: manager, WorkDir: workDir},
		Status: func(connected bool, err error) {
			if !connected && err != nil {
				log.Printf("not connected: %v", err)
			}
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Printf("NeoChat %s: %s\nThis computer: %s\nWorking folder: %s\nMCP config: %s\n", version, cfg.Server, runner.Describe(), workDir, mcpPath)

	if len(mcpConfig.Servers) > 0 {
		go func() {
			manager.Start(ctx)
			for _, s := range manager.Servers() {
				if s.Error != "" {
					log.Printf("MCP %s: not running: %s", s.Name, s.Error)
				} else {
					log.Printf("MCP %s: %d tools", s.Name, len(s.Tools))
				}
			}
			client.ToolsChanged()
		}()
	}

	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	if !*noWindow {
		windowClosed := openWindow(ctx, cfg.Server, filepath.Join(dir, "browser"))
		select {
		case <-windowClosed:
			log.Print("the window was closed: disconnecting this computer")
			stop()
		case err := <-done:
			reportExit(err)
			return
		case <-ctx.Done():
		}
		<-done
		return
	}
	reportExit(<-done)
}

func reportExit(err error) {
	var authErr *device.AuthError
	if errors.As(err, &authErr) {
		fatal(fmt.Errorf("%v: check the key with -key nc_...", err))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
}

func seconds(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Second
}

// loadConfig reads config.json, takes the flags into it, asks for what is
// still missing, and saves it.
func loadConfig(dir, server, key string) (config, error) {
	path := filepath.Join(dir, "config.json")
	var cfg config
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	changed := false
	if server != "" {
		cfg.Server, changed = server, true
	}
	if key != "" {
		cfg.Key, changed = key, true
	}
	in := bufio.NewReader(os.Stdin)
	if cfg.Server == "" {
		fmt.Print("NeoChat server address (e.g. https://chat.example.com): ")
		cfg.Server, changed = readLine(in), true
	}
	if cfg.Key == "" {
		fmt.Print("Your NeoChat API key (nc_...): ")
		cfg.Key, changed = readLine(in), true
	}
	cfg.Server = strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	if !strings.Contains(cfg.Server, "://") && cfg.Server != "" {
		cfg.Server = "https://" + cfg.Server
	}
	cfg.Key = strings.TrimSpace(cfg.Key)
	if cfg.Server == "" || cfg.Key == "" {
		return cfg, errors.New("the server address and the API key are both needed")
	}
	if _, err := device.ConnectURL(cfg.Server); err != nil {
		return cfg, err
	}
	if changed {
		data, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func readLine(in *bufio.Reader) string {
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func fatal(err error) {
	log.Print(err)
	fmt.Fprintln(os.Stderr, "\nNeoChat:", err)
	if isConsoleWindow() {
		fmt.Fprintln(os.Stderr, "Press Enter to close.")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
}
