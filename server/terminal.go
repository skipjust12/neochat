package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"neochat/attachment"
	"neochat/conversation"
	"neochat/device"
	"neochat/provider"
	"neochat/terminal"
)

// Terminal access: with Server.Terminal set, function-calling models of
// the users it allows get two more tools, and run code on this machine
// instead of guessing at what it would do:
//   - run_command runs a shell command;
//   - run_script saves a Python or bash script the model wrote and runs it.
//
// Both run in a working directory of the chat's own that outlives the
// answer, so a later command finds the files an earlier one wrote. The
// model sees the exit code and the output (its start and end, when long)
// and goes on. Each step streams as an activity: "Writing a script for …"
// while the model is still writing a script (from the tool call as it
// streams), "Running a command" / "Running a script" while it runs, and
// "Ran a command" once it is done; opened, the thinking block shows the
// command or the code, the exit code and the output.
const (
	terminalMaxRuns       = 20     // run_command + run_script calls per answer
	terminalMaxRounds     = 24     // model calls per answer with the terminal on
	terminalModelOutput   = 20_000 // characters of output the model gets
	terminalShownOutput   = 6_000  // characters of output kept for display
	terminalShownCommand  = 20_000 // characters of a command or script kept
	terminalHistoryNoteCh = 160    // characters of a command in the history note
)

var terminalFunctionTools = []provider.FunctionTool{
	{
		Name:        "run_command",
		Description: "Run a shell command on the server's terminal (bash -c) and get its exit code and output (stdout and stderr together). Use it to inspect files, run programs, install packages, check the system, or run a one-liner. It runs in this chat's own working directory, which keeps its files between calls. There is no stdin: nothing interactive (use flags like -y, avoid pagers and editors).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":         map[string]any{"type": "string", "description": "The command line to run."},
				"timeout_seconds": map[string]any{"type": "integer", "description": "Time limit in seconds; omit for the default."},
			},
			"required": []string{"command"},
		},
	},
	{
		Name:        "run_script",
		Description: "Write a script and run it on the server's terminal, then get its exit code and output. Prefer this over guessing whenever the answer depends on computing, parsing, converting or checking something: write the script, run it, read the output, and answer from it. Print what you need to see. The script runs in this chat's working directory; files it writes stay there for later calls.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				// "about" sorts first, so the model writes it before the code
				// and the UI can say what the script is for while it is
				// still being written.
				"about":           map[string]any{"type": "string", "description": "What the script is for, in a few words, so it reads after \"Writing a script for\" (e.g. \"counting the words in report.txt\"). Write this first."},
				"language":        map[string]any{"type": "string", "enum": []string{terminal.Python, terminal.Bash}, "description": "python (Python 3) or bash."},
				"code":            map[string]any{"type": "string", "description": "The whole script."},
				"timeout_seconds": map[string]any{"type": "integer", "description": "Time limit in seconds; omit for the default."},
			},
			"required": []string{"about", "language", "code"},
		},
	},
}

// sendFileTool hands the user a file from the terminal: the only way a
// file made there reaches them.
var sendFileTool = provider.FunctionTool{
	Name:        "send_file",
	Description: "Send the user a file you made on the terminal (a page, a chart, a spreadsheet, a document, an archive...). It appears under your answer as a file they can open or download; an .html page opens in the page viewer, a picture is shown. This is the only way to give the user a file: they can't open paths on the server, and sandbox: or file: links don't work. Call it once the file is final.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "The file's path: relative to the working directory, or absolute."},
			"name": map[string]any{"type": "string", "description": "The file name the user sees; omit to keep the file's own."},
		},
		"required": []string{"path"},
	},
}

// Limits on send_file.
const (
	maxSentFiles     = 10
	maxSentFileBytes = 25 << 20
)

// terminalToolsNote goes to every tool-calling model with the terminal:
// the facts of this machine. When and how to use it is in the core prompt
// (prompts/*.md, "The terminal"); the short version here covers a persona
// without it.
const terminalToolsNote = "You have a terminal on the server this chat runs on: run_command runs a shell command, run_script writes a Python or bash script and runs it. Machine: %s. Working directory (this chat's own, kept between runs and messages): %s. Time limit per run: %s by default, up to %s with timeout_seconds. To give the user a file you made, call send_file with its path; never write server paths or sandbox:/file: links, the user can't open them. Use it instead of guessing whenever the answer depends on computing, running or checking something; read the output, fix and rerun on errors, then answer from what it printed."

// noTerminalNote goes to a model that gets no terminal for this answer,
// since the core prompt describes one: without it, a model may claim to
// have run code.
const noTerminalNote = "You have no terminal for this answer (run_command and run_script aren't available to this model here), so you can't run code: work it out yourself, say when a result is unchecked, and never claim you ran anything."

// promptDescribesTerminal reports whether the leading system messages
// (the persona's core prompt) tell the model about the terminal, which a
// model without one then needs corrected (noTerminalNote).
func promptDescribesTerminal(messages []provider.Message) bool {
	for _, m := range messages {
		if m.Role != "system" {
			break
		}
		if strings.Contains(m.Content, "run_command") {
			return true
		}
	}
	return false
}

func isTerminalTool(name string) bool {
	return name == "run_command" || name == "run_script"
}

// terminalAllowed reports whether this answer has a terminal, on the
// server or on the user's computer (resolveTarget).
func (r *webRun) terminalAllowed() bool { return r.target != "" }

// --- the script the model is still writing ---

var aboutArgument = regexp.MustCompile(`"about"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// partialAbout is the script's purpose once the model has written it out
// whole in the streamed arguments; "" until then.
func partialAbout(arguments string) string {
	m := aboutArgument.FindStringSubmatch(arguments)
	if m == nil {
		return ""
	}
	var about string
	if json.Unmarshal([]byte(`"`+m[1]+`"`), &about) != nil {
		return ""
	}
	return cleanPurpose(about)
}

func cleanPurpose(about string) string {
	about = strings.Join(strings.Fields(about), " ")
	about = strings.TrimRight(about, ".…")
	return clipRunes(about, 160)
}

// onToolProgress shows a run_script call as "Writing a script for …" while
// the model is still writing it. The step is taken over by planTool once
// the call is complete (claimWriting); one that never completes is dropped
// (dropWriting).
func (r *webRun) onToolProgress(p provider.ToolCallProgress) {
	if p.Name != "run_script" || !r.terminalAllowed() {
		return
	}
	about := partialAbout(p.Arguments)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writing == nil {
		r.writing = map[int]int{}
	}
	index, ok := r.writing[p.Index]
	if !ok {
		r.activity = append(r.activity, conversation.ToolActivity{Tool: "run_script", Purpose: about, Pending: true, Phase: "writing"})
		r.writing[p.Index] = len(r.activity) - 1
		r.emitLocked()
		return
	}
	if step := &r.activity[index]; step.Purpose != about && about != "" {
		step.Purpose = about
		r.emitLocked()
	}
}

// claimWriting takes the "writing" step shown for call position i, if
// there is one; -1 if not.
func (r *webRun) claimWriting(i int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	index, ok := r.writing[i]
	if !ok {
		return -1
	}
	delete(r.writing, i)
	return index
}

// dropWriting removes the "writing" steps no call took: the model call
// failed, or ended without them.
func (r *webRun) dropWriting() { r.pruneWriting(nil) }

// pruneWriting removes the "writing" steps whose call position keep
// rejects (nil: all of them). They are the last steps shown, since
// nothing else starts while the model writes, so this runs before any
// step of the tool calls starts.
func (r *webRun) pruneWriting(keep func(position int) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	drop := map[int]bool{}
	for position, index := range r.writing {
		if keep == nil || !keep(position) {
			drop[index] = true
			delete(r.writing, position)
		}
	}
	if len(drop) == 0 {
		return
	}
	kept := r.activity[:0]
	shift := make([]int, len(r.activity))
	for i, step := range r.activity {
		shift[i] = len(kept)
		if !drop[i] {
			kept = append(kept, step)
		}
	}
	r.activity = kept
	for position, index := range r.writing {
		r.writing[position] = shift[index]
	}
	r.emitLocked()
}

// --- running ---

type terminalArgs struct {
	Command        string `json:"command"`
	Shell          string `json:"shell"`
	About          string `json:"about"`
	Language       string `json:"language"`
	Code           string `json:"code"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// planTerminalTool is planTool for run_command and run_script: an
// immediate result (bad arguments, limit reached) or the job that runs it.
// position is the call's place in its step, for the script shown while it
// was written.
func (r *webRun) planTerminalTool(call provider.ToolCall, position int) (string, func(context.Context) string) {
	writing := -1
	if call.Name == "run_script" {
		writing = r.claimWriting(position)
	}
	fail := func(message string) (string, func(context.Context) string) {
		if writing >= 0 {
			r.finish(writing, func(a *conversation.ToolActivity) { a.Error = message })
		}
		return "Error: " + message, nil
	}
	if !r.terminalAllowed() {
		return fail("the terminal is not available")
	}
	var args terminalArgs
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return fail("the arguments are not valid JSON")
	}
	step := conversation.ToolActivity{Tool: call.Name, Phase: "running"}
	if r.onComputer() {
		step.Where = executeOnComputer
	}
	switch call.Name {
	case "run_command":
		args.Command = strings.TrimSpace(args.Command)
		if args.Command == "" {
			return fail("command is empty")
		}
		args.Shell = strings.ToLower(strings.TrimSpace(args.Shell))
		if !r.onComputer() {
			args.Shell = ""
		} else if args.Shell != "" && !containsString(r.dev.info().Shells, args.Shell) {
			return fail(fmt.Sprintf("shell must be one of %s, not %q", strings.Join(r.dev.info().Shells, ", "), args.Shell))
		}
		step.Command, step.Shell = clipRunes(args.Command, terminalShownCommand), args.Shell
	case "run_script":
		args.Language = normalizeLanguage(args.Language)
		if strings.TrimSpace(args.Code) == "" {
			return fail("code is empty")
		}
		if languages := r.scriptLanguages(); !containsString(languages, args.Language) {
			return fail(fmt.Sprintf("language must be one of %s, not %q", strings.Join(languages, ", "), args.Language))
		}
		step.Language, step.Purpose, step.Command = args.Language, cleanPurpose(args.About), clipRunes(args.Code, terminalShownCommand)
	}
	if !r.take(&r.runs, terminalMaxRuns) {
		message := fmt.Sprintf("Terminal limit for this answer reached (%d runs). Answer with what you have.", terminalMaxRuns)
		if writing >= 0 {
			r.finish(writing, func(a *conversation.ToolActivity) { a.Error = "limit reached" })
		}
		return message, nil
	}
	var index int
	if writing >= 0 {
		index = writing
		r.update(index, func(a *conversation.ToolActivity) {
			pending := a.Pending
			*a = step
			a.Pending = pending
		})
	} else {
		index = r.start(step)
	}
	timeout := time.Duration(args.TimeoutSeconds) * time.Second
	return "", func(ctx context.Context) string {
		if !r.approve(ctx, index) {
			r.finish(index, func(a *conversation.ToolActivity) { a.Phase, a.Error = "", "denied" })
			return deniedResult
		}
		res, err := r.runCommand(ctx, call.Name, args, timeout)
		r.finish(index, func(a *conversation.ToolActivity) {
			a.Phase = ""
			code := res.ExitCode
			a.ExitCode = &code
			a.TimedOut = res.TimedOut
			a.DurationMS = res.Duration.Milliseconds()
			a.Output, a.OutputChars = shownOutput(res.Output), len([]rune(res.Output))
			if err != nil {
				a.Error = toolErrorMessage(err)
			}
		})
		if err != nil {
			return "Error: " + toolErrorMessage(err)
		}
		return formatRun(res)
	}
}

// update changes a step without ending it.
func (r *webRun) update(index int, change func(*conversation.ToolActivity)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	change(&r.activity[index])
	r.emitLocked()
}

// formatRun is what the model gets back from a run.
func formatRun(res terminal.Result) string {
	var b strings.Builder
	switch {
	case res.TimedOut:
		fmt.Fprintf(&b, "Killed: time limit reached after %s.\n", roundDuration(res.Duration))
	default:
		fmt.Fprintf(&b, "Exit code: %d (%s)\n", res.ExitCode, roundDuration(res.Duration))
	}
	output := res.Output
	if strings.TrimSpace(output) == "" {
		b.WriteString("(no output)")
		return b.String()
	}
	b.WriteString("Output:\n")
	b.WriteString(headAndTail(output, terminalModelOutput))
	if res.Truncated {
		b.WriteString("\n[Output was longer than the terminal keeps; the rest was dropped. Print less, or write it to a file and read parts of it.]")
	}
	return b.String()
}

func shownOutput(output string) string {
	return headAndTail(strings.TrimRight(output, "\n"), terminalShownOutput)
}

// headAndTail keeps the start and the end of a long text, which is where
// a command's output says what it did and how it ended.
func headAndTail(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	head, tail := n*2/3, n/3
	return string(runes[:head]) + fmt.Sprintf("\n… [%d characters left out] …\n", len(runes)-head-tail) + string(runes[len(runes)-tail:])
}

func roundDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// --- history ---

// terminalStepLine is a terminal step in webStepsNote.
func terminalStepLine(step conversation.ToolActivity) string {
	var line string
	switch step.Tool {
	case "send_file":
		line = "- sent the user the file " + oneLine(orDefault(step.Title, step.Command), 120)
	case "read_file":
		line = "- read the file " + oneLine(step.Title, 200)
	case "write_file":
		line = "- wrote the file " + oneLine(step.Title, 200)
	case "list_dir":
		line = "- listed the folder " + oneLine(step.Title, 200)
	case "mcp":
		line = "- used the MCP tool " + oneLine(step.Title, 120)
	case "run_script":
		line = "- wrote and ran a " + orDefault(step.Language, "script")
		if step.Language != "" {
			line += " script"
		}
		if step.Purpose != "" {
			line += " for " + oneLine(step.Purpose, 120)
		}
	default:
		line = "- ran the command `" + oneLine(step.Command, terminalHistoryNoteCh) + "`"
	}
	if step.Where == executeOnComputer {
		line += " on the user's computer"
	}
	switch {
	case step.Error == "denied":
		line += " (the user denied it, so it did not run)"
	case step.Error != "":
		line += " (failed: " + oneLine(step.Error, 120) + ")"
	case step.TimedOut:
		line += " (killed at the time limit)"
	case step.ExitCode != nil:
		line += fmt.Sprintf(" (exit code %d)", *step.ExitCode)
	}
	return line
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// boundTerminalStep keeps what a private chat's client sends back of a
// terminal step within the bounds the history note uses.
func boundTerminalStep(step conversation.ToolActivity) conversation.ToolActivity {
	kept := conversation.ToolActivity{
		Tool: step.Tool, Command: clipRunes(step.Command, 500), Purpose: clipRunes(step.Purpose, 160),
		Error: clipRunes(step.Error, 200), TimedOut: step.TimedOut,
		Title: clipRunes(step.Title, 200), Size: max(step.Size, 0),
	}
	if step.Where == executeOnComputer {
		kept.Where = executeOnComputer
	}
	if step.Language == terminal.Python || step.Language == terminal.Bash {
		kept.Language = step.Language
	}
	if step.ExitCode != nil {
		code := *step.ExitCode
		kept.ExitCode = &code
	}
	return kept
}

// --- sending files ---

// planSendFile is planTool for send_file.
func (r *webRun) planSendFile(call provider.ToolCall) (string, func(context.Context) string) {
	if !r.terminalAllowed() || r.s.Attachments == nil {
		return "Error: sending files is not available.", nil
	}
	var args struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || strings.TrimSpace(args.Path) == "" {
		return "Error: path is required.", nil
	}
	index := r.start(conversation.ToolActivity{Tool: "send_file", Command: clipRunes(strings.TrimSpace(args.Path), 1000)})
	return "", func(ctx context.Context) string {
		file, err := r.sendFile(ctx, args.Path, args.Name)
		r.finish(index, func(a *conversation.ToolActivity) {
			if err != nil {
				a.Error = err.Error()
				return
			}
			a.Title, a.Size = file.Name, file.Size
		})
		if err != nil {
			return "Error: " + err.Error()
		}
		return fmt.Sprintf("Sent %s (%s) to the user; it is shown under your answer. Mention it by name, don't link to it.", file.Name, sizeText(file.Size))
	}
}

// sendFile stores a file from the terminal as one of the user's files and
// adds it to the answer. The same file sent twice is sent once.
func (r *webRun) sendFile(ctx context.Context, path, name string) (conversation.Attachment, error) {
	if r.onComputer() {
		return r.sendComputerFile(ctx, path, name)
	}
	dir, err := r.s.Terminal.Dir(r.req.UserID, r.prepared.conversationID)
	if err != nil {
		return conversation.Attachment{}, err
	}
	real, err := resolveTerminalPath(dir, path)
	if err != nil {
		return conversation.Attachment{}, err
	}
	r.mu.Lock()
	if sent, ok := r.sentFrom[real]; ok {
		r.mu.Unlock()
		return sent, nil
	}
	if len(r.files) >= maxSentFiles {
		r.mu.Unlock()
		return conversation.Attachment{}, fmt.Errorf("at most %d files per answer", maxSentFiles)
	}
	r.mu.Unlock()

	info, err := os.Stat(real)
	switch {
	case err != nil:
		return conversation.Attachment{}, fmt.Errorf("no such file: %s", path)
	case info.IsDir():
		return conversation.Attachment{}, fmt.Errorf("%s is a directory: send its files, or pack it into an archive first", path)
	case !info.Mode().IsRegular():
		return conversation.Attachment{}, fmt.Errorf("%s is not a regular file", path)
	case info.Size() == 0:
		return conversation.Attachment{}, fmt.Errorf("%s is empty", path)
	case info.Size() > maxSentFileBytes:
		return conversation.Attachment{}, fmt.Errorf("%s is %s; files are limited to %s (compress or split it)", path, sizeText(info.Size()), sizeText(maxSentFileBytes))
	}
	data, err := os.ReadFile(real)
	if err != nil {
		return conversation.Attachment{}, fmt.Errorf("can't read %s: %v", path, err)
	}
	return r.storeSentFile(ctx, real, fileName(name, real), data)
}

// sendComputerFile is sendFile for a file on the user's computer.
func (r *webRun) sendComputerFile(ctx context.Context, path, name string) (conversation.Attachment, error) {
	path = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(path), "sandbox:"), "file://")
	r.mu.Lock()
	if len(r.files) >= maxSentFiles {
		r.mu.Unlock()
		return conversation.Attachment{}, fmt.Errorf("at most %d files per answer", maxSentFiles)
	}
	r.mu.Unlock()
	res, err := r.runOnComputer(ctx, device.Request{Op: device.OpFetchFile, Path: path})
	if err != nil {
		return conversation.Attachment{}, err
	}
	r.mu.Lock()
	if sent, ok := r.sentFrom[res.Path]; ok {
		r.mu.Unlock()
		return sent, nil
	}
	r.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		name = res.Name
	}
	return r.storeSentFile(ctx, res.Path, fileName(name, res.Name), res.Data)
}

// storeSentFile keeps a sent file with the user's files and adds it to the
// answer; real is where it came from, so it goes once.
func (r *webRun) storeSentFile(ctx context.Context, real, name string, data []byte) (conversation.Attachment, error) {
	kind, mimeType := sentFileKind(name, data)
	id := conversation.NewID()
	file := attachment.File{ID: id, UserID: r.req.UserID, Name: name, MIME: mimeType, Kind: kind, Size: int64(len(data)), Data: data, CreatedAt: time.Now()}
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := r.s.Attachments.Put(storeCtx, file); err != nil {
		log.Printf("server: store sent file for user_id=%s: %v", r.req.UserID, err)
		return conversation.Attachment{}, fmt.Errorf("couldn't store the file")
	}
	ref := conversation.Attachment{ID: id, Name: name, MIME: mimeType, Kind: kind, Size: file.Size}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sentFrom == nil {
		r.sentFrom = map[string]conversation.Attachment{}
	}
	r.sentFrom[real] = ref
	r.files = append(r.files, ref)
	return ref, nil
}

func (r *webRun) sentFiles() []conversation.Attachment {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]conversation.Attachment(nil), r.files...)
}

// resolveTerminalPath is a path the model gave, made absolute against the
// working directory, with the link schemes models invent (sandbox:,
// file://) taken off and symlinks followed.
func resolveTerminalPath(dir, path string) (string, error) {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "sandbox:")
	path = strings.TrimPrefix(path, "file://")
	if unescaped, err := url.PathUnescape(path); err == nil {
		path = unescaped
	}
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}
	if strings.HasPrefix(path, "~/") {
		path = filepath.Join(dir, path[2:])
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("no such file: %s", path)
	}
	return real, nil
}

func fileName(name, path string) string {
	name = strings.TrimSpace(filepath.Base(strings.TrimSpace(name)))
	if name == "" || name == "." || name == "/" {
		name = filepath.Base(path)
	}
	return clipRunes(name, 200)
}

// sentFileKind decides how the page shows a sent file: pictures as
// pictures, small text files (HTML pages too) as text it can open, the
// rest as downloads. The type comes from the bytes where they tell it,
// else from the name.
func sentFileKind(name string, data []byte) (kind, mimeType string) {
	if k, m, err := attachment.Classify(name, data); err == nil && (k == attachment.KindImage || k == attachment.KindText) {
		if k == attachment.KindText {
			if byName := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); strings.HasPrefix(byName, "text/") {
				m = strings.SplitN(byName, ";", 2)[0]
			}
		}
		return k, m
	}
	byName := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if byName == "" {
		byName = http.DetectContentType(data)
	}
	return attachment.KindFile, strings.SplitN(byName, ";", 2)[0]
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

// fileLink is a Markdown link, or a bare reference, to a file on this
// machine that the user can't open: sandbox:/..., file://..., or an
// absolute path under the terminal's directory.
var fileLink = regexp.MustCompile("\\[([^\\]\n]*)\\]\\(\\s*<?((?:sandbox:|file://)[^)>\\s]+)>?\\s*\\)|((?:sandbox:|file://)/[^\\s)\\]>\"'`]+)")

// rescueFileLinks fixes an answer that links to files on the server
// instead of sending them: each file that exists is sent, and the link
// becomes the file's name (the file is under the answer). A link to
// nothing becomes its text.
func (r *webRun) rescueFileLinks(ctx context.Context, text string) string {
	if !strings.Contains(text, "sandbox:") && !strings.Contains(text, "file://") {
		return text
	}
	return fileLink.ReplaceAllStringFunc(text, func(match string) string {
		m := fileLink.FindStringSubmatch(match)
		label, target := m[1], m[2]
		if target == "" {
			target = m[3]
		}
		file, err := r.sendFile(ctx, target, "")
		if err != nil {
			log.Printf("server: answer links to an unsendable file %q: %v", target, err)
			if label != "" {
				return label
			}
			return "`" + filepath.Base(strings.TrimPrefix(strings.TrimPrefix(target, "sandbox:"), "file://")) + "`"
		}
		if label != "" && label != file.Name {
			return label + " (**" + file.Name + "**, below)"
		}
		return "**" + file.Name + "** (below)"
	})
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
