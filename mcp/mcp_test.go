package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as a tiny MCP server: run with NEOCHAT_FAKE_MCP=1, the
// test binary speaks the protocol on stdin/stdout.
func TestMain(m *testing.M) {
	if os.Getenv("NEOCHAT_FAKE_MCP") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func fakeServer() {
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var msg struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
				Cursor    string         `json:"cursor"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil || msg.ID == nil {
			continue
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		switch msg.Method {
		case "initialize":
			// A server request in between, which the client must answer.
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "roots/list"})
			reply["result"] = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}}
		case "tools/list":
			if msg.Params.Cursor == "" {
				reply["result"] = map[string]any{"tools": []any{map[string]any{"name": "get_scene_info", "description": "Scene", "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": "2"}
			} else {
				reply["result"] = map[string]any{"tools": []any{map[string]any{"name": "add_cube", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"size": map[string]any{"type": "number"}}}}}}
			}
		case "tools/call":
			if msg.Params.Name == "add_cube" {
				reply["result"] = map[string]any{"content": []any{
					map[string]any{"type": "text", "text": fmt.Sprintf("added a cube of size %v", msg.Params.Arguments["size"])},
					map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
				}}
			} else {
				reply["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "no such tool"}}, "isError": true}
			}
		default:
			reply["error"] = map[string]any{"code": -32601, "message": "unknown"}
		}
		_ = out.Encode(reply)
	}
}

func fakeConfig(t *testing.T) ServerConfig {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Command: exe, Env: map[string]string{"NEOCHAT_FAKE_MCP": "1"}}
}

func TestClientListsAndCallsTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := NewManager(Config{Servers: map[string]ServerConfig{"blender": fakeConfig(t), "off": {Command: "x", Disabled: true}, "broken": {Command: "/no/such/program"}}})
	defer m.Close()
	m.Start(ctx)
	servers := m.Servers()
	if len(servers) != 2 {
		t.Fatalf("servers = %+v", servers)
	}
	var blender, broken Status
	for _, s := range servers {
		if s.Name == "blender" {
			blender = s
		} else {
			broken = s
		}
	}
	if len(blender.Tools) != 2 || blender.Tools[0].Name != "add_cube" || blender.Error != "" {
		t.Fatalf("blender = %+v", blender)
	}
	if broken.Error == "" {
		t.Fatal("a server that can't start should say why")
	}
	res, err := m.Call(ctx, "blender", "add_cube", json.RawMessage(`{"size":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Text(); !strings.Contains(text, "added a cube of size 2") || !strings.Contains(text, "[image: image/png") {
		t.Fatalf("text = %q", text)
	}
	res, err = m.Call(ctx, "blender", "nope", nil)
	if err != nil || !res.IsError {
		t.Fatalf("error result = %+v, %v", res, err)
	}
	if _, err := m.Call(ctx, "missing", "x", nil); err == nil {
		t.Fatal("a server that isn't configured should fail")
	}
}

func TestLoadConfig(t *testing.T) {
	path := t.TempDir() + "/mcp.json"
	if c, err := LoadConfig(path); err != nil || len(c.Servers) != 0 {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"blender":{"command":"uvx","args":["blender-mcp"]}}}`), 0o600)
	c, err := LoadConfig(path)
	if err != nil || c.Servers["blender"].Command != "uvx" || c.Servers["blender"].Args[0] != "blender-mcp" {
		t.Fatalf("config = %+v %v", c, err)
	}
}
