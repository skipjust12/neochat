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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
		Server: cfg.Server, Version: version,
		Executor: &device.Executor{Runner: runner, MCP: manager, WorkDir: workDir},
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

	// Connect first: a refused key is asked for again here, in the
	// console, and the window opens once the key works, signed in with it.
	var done chan error
	for {
		connected := make(chan struct{})
		var once sync.Once
		client.Key = cfg.Key
		client.Status = func(ok bool, err error) {
			if ok {
				once.Do(func() { close(connected) })
			} else if err != nil {
				log.Printf("not connected: %v", err)
			}
		}
		done = make(chan error, 1)
		go func() { done <- client.Run(ctx) }()
		fmt.Printf("Connecting to %s…\n", cfg.Server)
		select {
		case <-connected:
			fmt.Println("Connected. Models can now work on this computer when \"Execute commands on\" is set to Computer.")
		case <-time.After(15 * time.Second):
			fmt.Println("Still connecting (the server may be down); opening the window anyway.")
		case err := <-done:
			var authErr *device.AuthError
			if !errors.As(err, &authErr) {
				reportExit(err)
				return
			}
			fmt.Println("\nThe server didn't accept that key. Use your NeoChat key: the one you sign in to NeoChat with, nc_ and 64 letters and digits (not the Polza key, pza_...).")
			cfg.Key = askKey(bufio.NewReader(os.Stdin))
			if err := saveConfig(dir, cfg); err != nil {
				fatal(err)
			}
			continue
		case <-ctx.Done():
			return
		}
		break
	}

	if !*noWindow {
		windowClosed := openWindow(ctx, cfg.Server+"/#desktop-key="+url.QueryEscape(cfg.Key), filepath.Join(dir, "browser"))
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
	for cfg.Server == "" {
		fmt.Print("NeoChat server address (e.g. https://chat.example.com): ")
		cfg.Server, changed = readLine(in), true
	}
	cfg.Server = strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	if !strings.Contains(cfg.Server, "://") {
		cfg.Server = "https://" + cfg.Server
	}
	if _, err := device.ConnectURL(cfg.Server); err != nil {
		return cfg, err
	}
	cfg.Key = cleanKey(cfg.Key)
	if keyProblem(cfg.Key) != "" {
		if cfg.Key != "" {
			fmt.Println(keyProblem(cfg.Key))
		}
		cfg.Key, changed = askKey(in), true
	}
	if changed {
		if err := saveConfig(dir, cfg); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func saveConfig(dir string, cfg config) error {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(filepath.Join(dir, "config.json"), append(data, '\n'), 0o600)
}

// askKey asks for the NeoChat API key until it looks like one.
func askKey(in *bufio.Reader) string {
	for {
		fmt.Print("Your NeoChat API key (nc_...): ")
		key := cleanKey(readLine(in))
		problem := keyProblem(key)
		if problem == "" {
			return key
		}
		fmt.Println(problem)
	}
}

// cleanKey takes off what a paste tends to bring along: spaces, quotes, a
// "Bearer " in front.
func cleanKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.Trim(key, "\"'` ")
	key = strings.TrimPrefix(key, "Bearer ")
	return strings.TrimSpace(key)
}

// keyProblem says what is wrong with a key, "" when it looks right: nc_
// and 64 hex digits (auth's token format).
func keyProblem(key string) string {
	switch {
	case key == "":
		return "The key is empty."
	case strings.HasPrefix(key, "pza_"):
		return "That's a Polza key (pza_...). The app needs your NeoChat key, the one you sign in to NeoChat with (nc_...)."
	case !strings.HasPrefix(key, "nc_"):
		return "NeoChat keys start with nc_."
	case len(key) != 67:
		return fmt.Sprintf("That key is %d characters long; NeoChat keys are 67 (nc_ and 64 more). Was it cut off while pasting?", len(key))
	}
	for _, c := range key[3:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "NeoChat keys are nc_ and then digits and letters a-f only. Check that it was pasted whole."
		}
	}
	return ""
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
