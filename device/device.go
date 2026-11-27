// Package device is the protocol between the server and the NeoChat
// desktop app (cmd/neochat-desktop), which runs the model's tools on the
// user's own computer when the composer's "Execute commands on" is set
// to Computer.
//
// The app opens a WebSocket to the server (GET /device/connect, with the
// user's API key), so the computer needs no open port. It introduces
// itself with a Hello; the server then sends it Requests, one per tool
// call, and the app answers each with a Result carrying the same ID. A
// Cancel stops a request (the user pressed Stop). Both sides send Ping
// now and then to keep the connection and notice a dead one. Every
// message is one JSON object with a Type.
package device

import "encoding/json"

// Message types.
const (
	TypeHello  = "hello"
	TypeTools  = "tools" // the app's MCP tools changed
	TypeRun    = "run"
	TypeCancel = "cancel"
	TypeResult = "result"
	TypePing   = "ping"
	TypePong   = "pong"
)

// Operations a Request asks for.
const (
	OpRunCommand = "run_command"
	OpRunScript  = "run_script"
	OpReadFile   = "read_file"
	OpWriteFile  = "write_file"
	OpListDir    = "list_dir"
	OpFetchFile  = "fetch_file" // a file's bytes, for send_file
	OpMCPCall    = "mcp_call"
)

// Message is the envelope every message travels in; only the fields of
// its Type are set.
type Message struct {
	Type    string   `json:"type"`
	Hello   *Hello   `json:"hello,omitempty"`
	Tools   []Server `json:"tools,omitempty"`
	Request *Request `json:"request,omitempty"`
	Result  *Result  `json:"result,omitempty"`
	ID      string   `json:"id,omitempty"` // Cancel
}

// Hello is what the app says about the computer when it connects.
type Hello struct {
	Version string `json:"version"`
	// Name is the computer's name, OS e.g. "windows/amd64", Description
	// a line for the model (system, shells, Python).
	Name        string `json:"name"`
	OS          string `json:"os"`
	Description string `json:"description"`
	User        string `json:"user,omitempty"`
	Home        string `json:"home,omitempty"`
	// WorkDir holds the per-chat working directories.
	WorkDir   string   `json:"work_dir"`
	Shells    []string `json:"shells"`
	Languages []string `json:"languages"`
	// MCP are the app's MCP servers and their tools.
	MCP []Server `json:"mcp,omitempty"`
}

// Server is one MCP server the app runs, with its tools.
type Server struct {
	Name  string `json:"name"`
	Tools []Tool `json:"tools"`
	// Error is why the server isn't available (it failed to start).
	Error string `json:"error,omitempty"`
}

// Tool is one MCP tool: InputSchema is its arguments' JSON Schema.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Request is one tool call for the app to run. Chat is the conversation
// it belongs to, which picks the working directory.
type Request struct {
	ID        string `json:"id"`
	Op        string `json:"op"`
	Chat      string `json:"chat,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`

	// run_command: Shell ("" for the default) and Command. run_script:
	// Language and Code.
	Shell    string `json:"shell,omitempty"`
	Command  string `json:"command,omitempty"`
	Language string `json:"language,omitempty"`
	Code     string `json:"code,omitempty"`

	// Files: Path (relative to the working directory, or absolute);
	// read_file reads Limit characters from Offset; write_file writes
	// Content, appended with Append.
	Path    string `json:"path,omitempty"`
	Offset  int    `json:"offset,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Content string `json:"content,omitempty"`
	Append  bool   `json:"append,omitempty"`

	// mcp_call: the server, its tool and the arguments.
	Server    string          `json:"server,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Result is what a Request did. Error is set when it couldn't run at all
// (no such file, no such shell); a command that ran and failed has its
// exit code instead.
type Result struct {
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`

	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`

	// Files: the real path, the file's whole length in characters
	// (read_file) or bytes, and for fetch_file its bytes.
	Path  string `json:"path,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Chars int    `json:"chars,omitempty"`
	Data  []byte `json:"data,omitempty"`
	Name  string `json:"name,omitempty"`

	// mcp_call: the tool reported an error (its text is in Output).
	IsError bool `json:"is_error,omitempty"`
}

// MaxMessageBytes bounds one message: a fetched file (25 MB) in base64,
// with room to spare.
const MaxMessageBytes = 40 << 20

// Version is the protocol version the app and server speak.
const Version = "1"
