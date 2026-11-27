// Package mcp runs Model Context Protocol servers over stdio and calls
// their tools, for the desktop app (cmd/neochat-desktop): a server such as
// blender-mcp is a program the app starts, and talks to with JSON-RPC 2.0,
// one message per line, on its stdin and stdout.
//
// Config is the "mcpServers" file format of Claude Desktop and most MCP
// docs, so a server's own setup instructions apply as written.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// Config is a set of MCP servers by name:
//
//	{"mcpServers": {"blender": {"command": "uvx", "args": ["blender-mcp"]}}}
type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig starts one server: Command with Args, in Cwd, with Env
// added to the app's environment. Disabled leaves it off.
type ServerConfig struct {
	Command  string            `json:"command"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

// LoadConfig reads a config file; a missing file is an empty config.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Tool is one tool a server offers.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Content is one piece of a tool's result.
type Content struct {
	Type     string `json:"type"` // "text", "image", "audio", "resource", "resource_link"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64, for image and audio
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Resource *struct {
		URI  string `json:"uri"`
		Text string `json:"text,omitempty"`
	} `json:"resource,omitempty"`
}

// CallResult is what a tool call returned.
type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Text is the result as text for a model: text parts as they are, other
// parts named.
func (r CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s: %s, %d KB, not shown]", c.Type, c.MimeType, len(c.Data)*3/4/1024))
		case "resource":
			if c.Resource != nil && c.Resource.Text != "" {
				parts = append(parts, c.Resource.Text)
			} else if c.Resource != nil {
				parts = append(parts, "[resource "+c.Resource.URI+"]")
			}
		case "resource_link":
			parts = append(parts, "[link "+c.URI+"]")
		}
	}
	if len(parts) == 0 && len(r.StructuredContent) > 0 {
		return string(r.StructuredContent)
	}
	return strings.Join(parts, "\n")
}

// Client is one running server.
type Client struct {
	name string
	cmd  *exec.Cmd
	in   io.WriteCloser

	writeMu sync.Mutex
	nextID  atomic.Int64
	mu      sync.Mutex
	waiting map[int64]chan rpcResponse
	done    chan struct{}
	err     error
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Result json.RawMessage  `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Start launches a server and runs the MCP handshake.
func Start(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("mcp %s: no command", name)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Cwd
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	hideWindow(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &prefixLog{prefix: "mcp " + name + ": "}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: start %s: %w", name, cfg.Command, err)
	}
	c := &Client{name: name, cmd: cmd, in: in, waiting: map[int64]chan rpcResponse{}, done: make(chan struct{})}
	go c.read(out)
	go func() {
		err := cmd.Wait()
		c.fail(fmt.Errorf("mcp %s exited: %v", name, err))
	}()

	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(initCtx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "neochat", "version": "1"},
	}, &init); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w", name, err)
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Name is the server's name in the config.
func (c *Client) Name() string { return c.name }

// Alive reports whether the server is still running.
func (c *Client) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Tools lists the server's tools, all pages.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// Call runs one tool.
func (c *Client) Call(ctx context.Context, tool string, arguments json.RawMessage) (CallResult, error) {
	var args any = map[string]any{}
	if len(arguments) > 0 && string(arguments) != "null" {
		args = arguments
	}
	var res CallResult
	err := c.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, &res)
	return res, err
}

// Close stops the server.
func (c *Client) Close() {
	_ = c.in.Close()
	if c.cmd.Process != nil {
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
		}
	}
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.waiting[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}()
	if err := c.send(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return err
	}
	select {
	case res := <-ch:
		if res.Error != nil {
			return fmt.Errorf("%s (code %d)", res.Error.Message, res.Error.Code)
		}
		if out != nil && len(res.Result) > 0 {
			return json.Unmarshal(res.Result, out)
		}
		return nil
	case <-c.done:
		return c.err
	case <-ctx.Done():
		_ = c.notify("notifications/cancelled", map[string]any{"requestId": id, "reason": "cancelled"})
		return ctx.Err()
	}
}

func (c *Client) notify(method string, params any) error {
	return c.send(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) send(msg rpcRequest) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.in.Write(append(data, '\n'))
	return err
}

func (c *Client) read(out io.Reader) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 0, 64*1024), 64<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var res rpcResponse
		if json.Unmarshal([]byte(line), &res) != nil {
			log.Printf("mcp %s: not JSON-RPC: %.200s", c.name, line)
			continue
		}
		if res.ID == nil {
			continue // a notification (progress, logging): nothing waits on it
		}
		if res.Method != "" {
			// A request from the server (sampling, roots, ping): answer that
			// it isn't supported, except ping.
			c.answerServerRequest(*res.ID, res.Method)
			continue
		}
		var id int64
		if json.Unmarshal(*res.ID, &id) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.waiting[id]
		c.mu.Unlock()
		if ch != nil {
			ch <- res
		}
	}
	c.fail(fmt.Errorf("mcp %s closed its output", c.name))
}

func (c *Client) answerServerRequest(id json.RawMessage, method string) {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	if method == "ping" {
		reply["result"] = map[string]any{}
	} else {
		reply["error"] = map[string]any{"code": -32601, "message": "method not supported by this client"}
	}
	data, _ := json.Marshal(reply)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.in.Write(append(data, '\n'))
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
}

type prefixLog struct{ prefix string }

func (p *prefixLog) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			log.Print(p.prefix + line)
		}
	}
	return len(b), nil
}

// Manager runs every server of a config and routes calls to them,
// starting a server again when it has died.
type Manager struct {
	config  Config
	mu      sync.Mutex
	clients map[string]*Client
	errors  map[string]string
	tools   map[string][]Tool
}

// NewManager starts nothing yet; see Start.
func NewManager(config Config) *Manager {
	return &Manager{config: config, clients: map[string]*Client{}, errors: map[string]string{}, tools: map[string][]Tool{}}
}

// Start starts every enabled server and lists its tools. A server that
// fails is kept with its error.
func (m *Manager) Start(ctx context.Context) {
	names := make([]string, 0, len(m.config.Servers))
	for name, cfg := range m.config.Servers {
		if !cfg.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.start(ctx, name)
		}()
	}
	wg.Wait()
}

func (m *Manager) start(ctx context.Context, name string) (*Client, error) {
	client, err := Start(ctx, name, m.config.Servers[name])
	var tools []Tool
	if err == nil {
		listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		tools, err = client.Tools(listCtx)
		cancel()
		if err != nil {
			client.Close()
			client = nil
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		log.Printf("mcp %s: %v", name, err)
		m.errors[name] = err.Error()
		delete(m.clients, name)
		return nil, err
	}
	delete(m.errors, name)
	m.clients[name], m.tools[name] = client, tools
	log.Printf("mcp %s: %d tools", name, len(tools))
	return client, nil
}

// Status is every configured server with its tools, or why it's down.
type Status struct {
	Name  string
	Tools []Tool
	Error string
}

// Servers reports the servers' state.
func (m *Manager) Servers() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Status
	for name, cfg := range m.config.Servers {
		if cfg.Disabled {
			continue
		}
		out = append(out, Status{Name: name, Tools: m.tools[name], Error: m.errors[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Call runs a tool of a server, restarting the server once if it died.
func (m *Manager) Call(ctx context.Context, server, tool string, arguments json.RawMessage) (CallResult, error) {
	m.mu.Lock()
	client := m.clients[server]
	_, configured := m.config.Servers[server]
	m.mu.Unlock()
	if !configured {
		return CallResult{}, fmt.Errorf("no MCP server %q", server)
	}
	if client == nil || !client.Alive() {
		var err error
		if client, err = m.start(ctx, server); err != nil {
			return CallResult{}, err
		}
	}
	return client.Call(ctx, tool, arguments)
}

// Close stops every server.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		c.Close()
	}
	m.clients = map[string]*Client{}
}
