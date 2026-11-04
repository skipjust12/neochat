package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"neochat/conversation"
	"neochat/device"
	"neochat/provider"
	"neochat/terminal"
)

// Where the terminal tools run, and whether each action waits for the
// user (the composer's + menu: "Execute commands on" and "Ask for
// approval" / "Allow all").
const (
	executeOnServer   = "server"
	executeOnComputer = "computer"
	approvalAsk       = "ask"
	approvalAllow     = "allow"
)

// longToolTimeout bounds one terminal, file or MCP step, approval wait
// included; the runner's own time limit is shorter.
const longToolTimeout = 15 * time.Minute

// resolveTarget decides, once per answer, where its terminal tools run:
// on this server, on the user's connected computer, or nowhere.
func (r *webRun) resolveTarget() {
	if r.req.ExecuteOn == executeOnComputer {
		r.wantComputer = true
		if d := r.s.devices().device(r.req.UserID); d != nil {
			r.target, r.dev = executeOnComputer, d
			r.mcpByName = mcpToolNames(d.info().MCP)
		}
		return
	}
	if r.s.Terminal != nil && r.s.Terminal.Allowed(r.req.UserID) {
		r.target = executeOnServer
	}
}

func (r *webRun) onComputer() bool { return r.target == executeOnComputer }

// computerMissingNote: Computer chosen, but no app connected.
const computerMissingNote = "The user set commands to run on their own computer, but the NeoChat desktop app isn't connected right now, so you have no terminal for this answer and can't run anything. If the task needs it, tell them in one sentence to open the NeoChat app on their computer (or to switch \"Execute commands on\" to Server in the + menu), then help with what you can without it; never claim you ran anything."

const (
	approvalAskNote   = "The user approves each command, script, file write and MCP action before it runs (reading files and listing folders need no approval). A denied action comes back as denied: don't try the same thing another way; say what you wanted to do and ask. Approvals take the user's time, so batch work into fewer, well-thought-out steps."
	approvalAllowNote = "Actions run without asking the user first, so be careful: nothing destructive or irreversible they didn't ask for."
)

// terminalNote is the per-answer note about the terminal: where it is and
// how it's set up. When and how to use it is in the core prompt.
func (r *webRun) terminalNote() string {
	approval := approvalAllowNote
	if r.req.Approval == approvalAsk {
		approval = approvalAskNote
	}
	if r.onComputer() {
		h := r.dev.info()
		var b strings.Builder
		fmt.Fprintf(&b, "Your tools run on the user's own computer, %q (%s), not on a server: %s.", h.Name, h.OS, h.Description)
		if h.User != "" || h.Home != "" {
			fmt.Fprintf(&b, " User %s, home folder %s.", h.User, h.Home)
		}
		fmt.Fprintf(&b, " Each chat has its own working directory under %s (relative paths start there), kept between runs and messages; you can also work anywhere on the computer the task needs.", h.WorkDir)
		if len(h.Shells) > 0 {
			fmt.Fprintf(&b, " run_command uses %s unless you pick another shell (%s); write commands in that shell's syntax.", h.Shells[0], strings.Join(h.Shells, ", "))
		}
		if len(h.Languages) > 0 {
			fmt.Fprintf(&b, " run_script takes %s.", strings.Join(h.Languages, ", "))
		}
		b.WriteString(" read_file, write_file and list_dir work with files directly; send_file gives the user a file.")
		if names := mcpServerNames(h.MCP); names != "" {
			fmt.Fprintf(&b, " MCP tools (mcp__<server>__<tool>) control apps through the app's MCP servers: %s.", names)
		}
		b.WriteString(" It is the user's real machine with their files, programs and accounts: change, move or delete only what the task is about, and never touch system settings, other people's data or credentials unless asked. ")
		b.WriteString(approval)
		return b.String()
	}
	dir, err := r.s.Terminal.Dir(r.req.UserID, r.prepared.conversationID)
	if err != nil {
		dir = "unavailable: " + err.Error()
	}
	def, most := r.s.Terminal.TimeoutLimits()
	return fmt.Sprintf(terminalToolsNote, r.s.Terminal.Describe(), dir, def, most) + " " + approval
}

// terminalTools are the tools for this answer's terminal: on the server
// run_command, run_script and send_file; on a computer also the file
// tools and its MCP tools, with the shells and languages it has.
func (r *webRun) terminalTools() []provider.FunctionTool {
	if !r.onComputer() {
		tools := append([]provider.FunctionTool(nil), terminalFunctionTools...)
		if r.s.Attachments != nil {
			tools = append(tools, sendFileTool)
		}
		return tools
	}
	h := r.dev.info()
	command := terminalFunctionTools[0]
	command.Description = "Run a command on the user's computer and get its exit code and output (stdout and stderr together). It runs in this chat's working directory unless the command changes it. There is no stdin: nothing interactive."
	properties := map[string]any{
		"command":         map[string]any{"type": "string", "description": "The command line to run, in the shell's syntax."},
		"timeout_seconds": map[string]any{"type": "integer", "description": "Time limit in seconds; omit for the default."},
	}
	if len(h.Shells) > 0 {
		properties["shell"] = map[string]any{"type": "string", "enum": h.Shells, "description": "The shell; omit for " + h.Shells[0] + "."}
	}
	command.Parameters = map[string]any{"type": "object", "properties": properties, "required": []string{"command"}}

	script := terminalFunctionTools[1]
	script.Description = strings.Replace(script.Description, "Write a script and run it on the server's terminal", "Write a script and run it on the user's computer", 1)
	languages := h.Languages
	if len(languages) == 0 {
		languages = []string{terminal.Python}
	}
	script.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"about":           map[string]any{"type": "string", "description": "What the script is for, in a few words, so it reads after \"Writing a script for\" (e.g. \"renaming the photos by date\"). Write this first."},
			"language":        map[string]any{"type": "string", "enum": languages},
			"code":            map[string]any{"type": "string", "description": "The whole script."},
			"timeout_seconds": map[string]any{"type": "integer", "description": "Time limit in seconds; omit for the default."},
		},
		"required": []string{"about", "language", "code"},
	}
	tools := []provider.FunctionTool{command, script, computerFileTools[0], computerFileTools[1], computerFileTools[2]}
	if r.s.Attachments != nil {
		tools = append(tools, sendFileTool)
	}
	return append(tools, mcpFunctionTools(h.MCP, r.mcpByName)...)
}

var computerFileTools = []provider.FunctionTool{
	{
		Name:        "read_file",
		Description: "Read a text file on the user's computer. Returns up to 50,000 characters from offset; pass offset to read on. For binary files (Office documents, images, databases) use a script instead.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "Absolute, ~/..., or relative to the working directory."},
			"offset": map[string]any{"type": "integer", "description": "Character offset to start at."},
		}, "required": []string{"path"}},
	},
	{
		Name:        "write_file",
		Description: "Write a text file on the user's computer (UTF-8), creating its folders; replaces the file unless append is true. Read a file before you change it, and write the whole new content.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Absolute, ~/..., or relative to the working directory."},
			"content": map[string]any{"type": "string"},
			"append":  map[string]any{"type": "boolean", "description": "Add to the end instead of replacing."},
		}, "required": []string{"path", "content"}},
	},
	{
		Name:        "list_dir",
		Description: "List a folder on the user's computer: folders first, then files with size and date. Omit path for the working directory.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"path": map[string]any{"type": "string"},
		}},
	},
}

func isComputerFileTool(name string) bool {
	return name == "read_file" || name == "write_file" || name == "list_dir"
}

func isMCPTool(name string) bool { return strings.HasPrefix(name, "mcp__") }

// isLongTool: a step that may run for minutes (and wait for approval),
// not under webStepTimeout.
func isLongTool(name string) bool {
	return isTerminalTool(name) || isComputerFileTool(name) || isMCPTool(name) || name == "send_file"
}

// --- MCP ---

var toolNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// mcpToolNames gives each MCP tool a function name the vendors accept
// (mcp__<server>__<tool>, at most 64 characters, unique) and maps it back.
func mcpToolNames(servers []device.Server) map[string][2]string {
	out := map[string][2]string{}
	for _, server := range servers {
		for _, tool := range server.Tools {
			name := "mcp__" + toolNameUnsafe.ReplaceAllString(server.Name, "_") + "__" + toolNameUnsafe.ReplaceAllString(tool.Name, "_")
			if len(name) > 64 {
				name = name[:64]
			}
			base := name
			for n := 2; ; n++ {
				if _, taken := out[name]; !taken {
					break
				}
				suffix := fmt.Sprintf("_%d", n)
				name = base[:min(len(base), 64-len(suffix))] + suffix
			}
			out[name] = [2]string{server.Name, tool.Name}
		}
	}
	return out
}

func mcpFunctionTools(servers []device.Server, names map[string][2]string) []provider.FunctionTool {
	byPair := map[[2]string]device.Tool{}
	for _, server := range servers {
		for _, tool := range server.Tools {
			byPair[[2]string{server.Name, tool.Name}] = tool
		}
	}
	keys := make([]string, 0, len(names))
	for name := range names {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	var out []provider.FunctionTool
	for _, name := range keys {
		pair := names[name]
		tool := byPair[pair]
		params := map[string]any{}
		if len(tool.InputSchema) > 0 && json.Unmarshal(tool.InputSchema, &params) != nil {
			params = map[string]any{}
		}
		if params["type"] != "object" {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if _, ok := params["properties"]; !ok {
			params["properties"] = map[string]any{}
		}
		description := "[MCP server " + pair[0] + "] " + tool.Description
		out = append(out, provider.FunctionTool{Name: name, Description: clipRunes(strings.TrimSpace(description), 1000), Parameters: params})
	}
	return out
}

func mcpServerNames(servers []device.Server) string {
	var names []string
	for _, s := range servers {
		if s.Error != "" {
			names = append(names, s.Name+" (not running: "+clipRunes(s.Error, 80)+")")
		} else {
			names = append(names, fmt.Sprintf("%s (%d tools)", s.Name, len(s.Tools)))
		}
	}
	return strings.Join(names, ", ")
}

// --- running on the computer ---

// runOnComputer runs one request on the user's computer.
func (r *webRun) runOnComputer(ctx context.Context, req device.Request) (device.Result, error) {
	req.Chat = r.prepared.conversationID
	res, err := r.dev.call(ctx, req)
	if err != nil {
		return res, err
	}
	if res.Error != "" && res.Error != "stopped" {
		return res, &terminal.Error{Message: res.Error}
	}
	return res, nil
}

// runCommand runs run_command or run_script where this answer's terminal
// is.
func (r *webRun) runCommand(ctx context.Context, name string, args terminalArgs, timeout time.Duration) (terminal.Result, error) {
	if r.onComputer() {
		req := device.Request{Op: device.OpRunCommand, Shell: args.Shell, Command: args.Command, TimeoutMS: timeout.Milliseconds()}
		if name == "run_script" {
			req = device.Request{Op: device.OpRunScript, Language: args.Language, Code: args.Code, TimeoutMS: timeout.Milliseconds()}
		}
		res, err := r.runOnComputer(ctx, req)
		out := terminal.Result{ExitCode: res.ExitCode, Output: res.Output, Truncated: res.Truncated, TimedOut: res.TimedOut, Duration: time.Duration(res.DurationMS) * time.Millisecond}
		if err != nil && res.ExitCode == 0 {
			out.ExitCode = -1
		}
		return out, err
	}
	runner := r.s.Terminal
	dir, err := runner.Dir(r.req.UserID, r.prepared.conversationID)
	if err != nil {
		return terminal.Result{ExitCode: -1}, err
	}
	if name == "run_command" {
		return runner.Run(ctx, dir, args.Command, timeout)
	}
	_, res, err := runner.RunScript(ctx, dir, args.Language, args.Code, timeout)
	return res, err
}

// scriptLanguages are the run_script languages of this answer's terminal.
func (r *webRun) scriptLanguages() []string {
	if r.onComputer() {
		if langs := r.dev.info().Languages; len(langs) > 0 {
			return langs
		}
		return []string{terminal.Python}
	}
	return []string{terminal.Python, terminal.Bash}
}

func normalizeLanguage(language string) string {
	switch language = strings.ToLower(strings.TrimSpace(language)); language {
	case "python3", "py":
		return terminal.Python
	case "sh", "shell", "zsh":
		return terminal.Bash
	case "ps", "ps1", "pwsh", "powershell.exe":
		return terminal.PowerShell
	case "bat", "batch", "cmd.exe":
		return terminal.Cmd
	}
	return language
}

// --- approvals ---

// approve asks the user to allow step index before it runs (in "ask"
// mode) and waits for the answer: true to go ahead.
func (r *webRun) approve(ctx context.Context, index int) bool {
	if r.req.Approval != approvalAsk {
		return true
	}
	hub := r.s.approvals()
	id, answer := hub.open(r.req.UserID)
	defer hub.close(id)
	r.update(index, func(a *conversation.ToolActivity) {
		a.Approval, a.ApprovalID = "pending", id
	})
	allowed := false
	select {
	case allowed = <-answer:
	case <-ctx.Done():
	case <-time.After(approvalTimeout):
	}
	r.update(index, func(a *conversation.ToolActivity) {
		a.ApprovalID = ""
		if allowed {
			a.Approval = "allowed"
		} else {
			a.Approval = "denied"
		}
	})
	return allowed
}

// deniedResult is the tool result of an action the user didn't allow.
const deniedResult = "The user denied this action, so it did not run. Don't try to do the same thing another way; tell the user what you wanted to do and why, and ask how they want to proceed."

// --- file and MCP tools ---

// planComputerTool is planTool for read_file, write_file, list_dir and
// the MCP tools.
func (r *webRun) planComputerTool(call provider.ToolCall) (string, func(context.Context) string) {
	if !r.onComputer() {
		return "Error: this tool works on the user's computer only, and it isn't connected.", nil
	}
	var args struct {
		Path    string `json:"path"`
		Offset  int    `json:"offset"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if !isMCPTool(call.Name) && strings.TrimSpace(call.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return "Error: the arguments are not valid JSON.", nil
		}
	}
	if !r.take(&r.runs, terminalMaxRuns) {
		return fmt.Sprintf("Terminal limit for this answer reached (%d actions). Answer with what you have.", terminalMaxRuns), nil
	}
	step := conversation.ToolActivity{Tool: call.Name, Where: executeOnComputer, Phase: "running"}
	var req device.Request
	needsApproval := false
	switch call.Name {
	case "read_file":
		if strings.TrimSpace(args.Path) == "" {
			return "Error: path is required.", nil
		}
		step.Title = clipRunes(args.Path, 500)
		req = device.Request{Op: device.OpReadFile, Path: args.Path, Offset: args.Offset}
	case "write_file":
		if strings.TrimSpace(args.Path) == "" {
			return "Error: path is required.", nil
		}
		step.Title, step.Command, step.Size = clipRunes(args.Path, 500), clipRunes(args.Content, terminalShownCommand), int64(len(args.Content))
		step.Purpose = map[bool]string{true: "append"}[args.Append]
		req = device.Request{Op: device.OpWriteFile, Path: args.Path, Content: args.Content, Append: args.Append}
		needsApproval = true
	case "list_dir":
		step.Title = clipRunes(orDefault(strings.TrimSpace(args.Path), "the working directory"), 500)
		req = device.Request{Op: device.OpListDir, Path: args.Path}
	default:
		pair, ok := r.mcpByName[call.Name]
		if !ok {
			return fmt.Sprintf("Error: there is no tool named %q.", call.Name), nil
		}
		raw := json.RawMessage(strings.TrimSpace(call.Arguments))
		if len(raw) == 0 {
			raw = json.RawMessage("{}")
		}
		if !json.Valid(raw) {
			return "Error: the arguments are not valid JSON.", nil
		}
		step.Tool, step.Title, step.Command = "mcp", pair[0]+" · "+pair[1], clipRunes(prettyJSON(raw), terminalShownCommand)
		req = device.Request{Op: device.OpMCPCall, Server: pair[0], Tool: pair[1], Arguments: raw}
		needsApproval = true
	}
	index := r.start(step)
	return "", func(ctx context.Context) string {
		if needsApproval && !r.approve(ctx, index) {
			r.finish(index, func(a *conversation.ToolActivity) { a.Phase, a.Error = "", "denied" })
			return deniedResult
		}
		res, err := r.runOnComputer(ctx, req)
		r.finish(index, func(a *conversation.ToolActivity) {
			a.Phase = ""
			a.DurationMS = res.DurationMS
			if res.Path != "" && call.Name != "write_file" {
				a.Title = clipRunes(res.Path, 500)
			}
			if err != nil {
				a.Error = toolErrorMessage(err)
				return
			}
			switch call.Name {
			case "read_file", "list_dir":
				a.Output, a.OutputChars = shownOutput(res.Output), max(res.Chars, len([]rune(res.Output)))
			case "write_file":
				a.Title, a.Size = clipRunes(orDefault(res.Path, a.Title), 500), res.Size
			default:
				a.Output, a.OutputChars = shownOutput(res.Output), len([]rune(res.Output))
				if res.IsError {
					a.Error = "the tool reported an error"
				}
			}
		})
		if err != nil {
			return "Error: " + toolErrorMessage(err)
		}
		switch call.Name {
		case "read_file":
			head := fmt.Sprintf("File: %s (%d characters)\n", res.Path, res.Chars)
			if res.Truncated {
				end := args.Offset + len([]rune(res.Output))
				head += fmt.Sprintf("[Characters %d-%d of %d. Call read_file with offset=%d to read on.]\n", args.Offset, end, res.Chars, end)
			}
			return head + "\n" + res.Output
		case "write_file":
			return fmt.Sprintf("Wrote %s (%s).", res.Path, sizeText(res.Size))
		case "list_dir":
			return "Folder: " + res.Path + "\n\n" + res.Output
		}
		text := orDefault(res.Output, "(no output)")
		if res.IsError {
			text = "The tool reported an error:\n" + text
		}
		if res.Truncated {
			text += "\n[The result was longer and was cut.]"
		}
		return text
	}
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}
