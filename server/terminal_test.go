package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"neochat/conversation"
	"neochat/provider"
	"neochat/terminal"
)

// partialToolCallChunk is one streamed piece of a tool call: the first
// piece carries id and name, later ones only more arguments.
func partialToolCallChunk(index int, id, name, arguments string, last bool) string {
	call := map[string]any{"index": index, "function": map[string]any{"arguments": arguments}}
	if id != "" {
		call["id"] = id
		call["function"].(map[string]any)["name"] = name
	}
	choice := map[string]any{"delta": map[string]any{"tool_calls": []any{call}}}
	if last {
		choice["finish_reason"] = "tool_calls"
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{choice}})
	return string(b)
}

func newTerminalServer(t *testing.T, vendor *scriptedPolza, users ...string) *Server {
	t.Helper()
	s := newWebServer(t, vendor)
	s.Terminal = &terminal.Runner{WorkDir: t.TempDir(), Users: users}
	return s
}

func TestTerminalScriptRunsAndShowsItsSteps(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if len(toolMessages(body)) == 0 {
			return 200, []string{
				partialToolCallChunk(0, "r1", "run_script", `{"about":"adding `, false),
				partialToolCallChunk(0, "", "", `two numbers","language":"bash",`, false),
				partialToolCallChunk(0, "", "", `"code":"echo $((40+2)) > n.txt; cat n.txt"}`, true),
				usageChunk,
			}
		}
		return 200, []string{textChunk("It is 42."), usageChunk}
	}}
	s := newTerminalServer(t, vendor, "u1")
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "40+2?", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "off"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if done.ResponseText != "It is 42." || len(done.Activity) != 1 {
		t.Fatalf("done = %q, %+v", done.ResponseText, done.Activity)
	}
	step := done.Activity[0]
	if step.Tool != "run_script" || step.Language != "bash" || step.Purpose != "adding two numbers" || step.ExitCode == nil || *step.ExitCode != 0 || step.Output != "42" || step.Pending || step.Phase != "" {
		t.Fatalf("step = %+v", step)
	}

	// Live: "writing" (with the purpose, once it is whole) before "running".
	var phases []string
	sawPurposeWhileWriting := false
	for _, e := range events["activity"] {
		for _, s := range e.(map[string]any)["activity"].([]conversation.ToolActivity) {
			if s.Pending && (len(phases) == 0 || phases[len(phases)-1] != s.Phase) {
				phases = append(phases, s.Phase)
			}
			if s.Phase == "writing" && s.Purpose == "adding two numbers" {
				sawPurposeWhileWriting = true
			}
		}
	}
	if len(phases) < 2 || phases[0] != "writing" || phases[len(phases)-1] != "running" || !sawPurposeWhileWriting {
		t.Fatalf("phases = %v, purpose while writing %v", phases, sawPurposeWhileWriting)
	}

	main := vendor.mainCalls()
	if names := strings.Join(toolNames(main[0]), ","); names != "run_command,run_script,send_file,ask_user" {
		t.Fatalf("tools = %s", names)
	}
	if !hasSystemPrefix(main[0], "You have a terminal on the server") {
		t.Fatal("no terminal note")
	}
	result := toolMessages(main[1])[0]["content"].(string)
	if !strings.HasPrefix(result, "Exit code: 0") || !strings.Contains(result, "42") {
		t.Fatalf("tool result = %q", result)
	}
}

func TestTerminalCommandKeepsFilesForTheChat(t *testing.T) {
	round := 0
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		switch len(toolMessages(body)) {
		case 0:
			round++
			return 200, []string{toolCallChunk([3]string{"c1", "run_command", `{"command":"echo hi > f.txt && false"}`}), usageChunk}
		case 1:
			return 200, []string{toolCallChunk([3]string{"c2", "run_command", `{"command":"cat f.txt"}`}), usageChunk}
		}
		return 200, []string{textChunk("Done."), usageChunk}
	}}
	s := newTerminalServer(t, vendor, "*")
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "go", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if len(done.Activity) != 2 {
		t.Fatalf("activity = %+v", done.Activity)
	}
	first, second := done.Activity[0], done.Activity[1]
	if first.Tool != "run_command" || first.Command != "echo hi > f.txt && false" || *first.ExitCode != 1 {
		t.Fatalf("first = %+v", first)
	}
	if second.Output != "hi" || *second.ExitCode != 0 {
		t.Fatalf("second = %+v", second)
	}
	main := vendor.mainCalls()
	if got := toolMessages(main[1])[0]["content"].(string); !strings.HasPrefix(got, "Exit code: 1") || !strings.Contains(got, "(no output)") {
		t.Fatalf("first result = %q", got)
	}
}

func TestTerminalIsOnlyForAllowedUsers(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		return 200, []string{textChunk("Hi."), usageChunk}
	}}
	s := newTerminalServer(t, vendor, "someone-else")
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	main := vendor.mainCalls()
	for _, name := range toolNames(main[0]) {
		if isTerminalTool(name) {
			t.Fatalf("tools = %v", toolNames(main[0]))
		}
	}
	if hasSystemPrefix(main[0], "You have a terminal") {
		t.Fatal("terminal note for a user without the terminal")
	}

	// And a call made anyway is refused.
	r := &webRun{s: s, req: req}
	if result, job := r.planTool(toolCall("run_command", `{"command":"id"}`), 0); job != nil || !strings.Contains(result, "not available") {
		t.Fatalf("result = %q", result)
	}
}

func TestTerminalWritingStepIsDroppedWhenTheCallNeverComes(t *testing.T) {
	r := &webRun{s: &Server{Terminal: &terminal.Runner{Users: []string{"*"}}}, req: chatRequest{UserID: "u1"}, target: executeOnServer}
	r.start(conversation.ToolActivity{Tool: "web_search", Query: "q"})
	r.onToolProgress(toolProgress(0, "run_script", `{"about":"x"`))
	r.onToolProgress(toolProgress(1, "run_script", `{"ab`))
	if len(r.activity) != 3 || r.activity[1].Purpose != "x" || r.activity[2].Phase != "writing" {
		t.Fatalf("activity = %+v", r.activity)
	}
	// Only the second call came out as run_script.
	r.pruneWriting(func(i int) bool { return i == 1 })
	if len(r.activity) != 2 || r.activity[1].Purpose != "" || r.writing[1] != 1 {
		t.Fatalf("after prune: %+v, %v", r.activity, r.writing)
	}
	r.dropWriting()
	if len(r.activity) != 1 || r.activity[0].Tool != "web_search" {
		t.Fatalf("after drop: %+v", r.activity)
	}
}

func TestPartialAbout(t *testing.T) {
	for args, want := range map[string]string{
		`{"about":"counting \"words\".`:         "",
		`{"about":"counting \"words\"."`:        `counting "words"`,
		`{"language":"python","about" : "a  b"`: "a b",
		`{"code":"print(1)"`:                    "",
	} {
		if got := partialAbout(args); got != want {
			t.Errorf("partialAbout(%s) = %q, want %q", args, got, want)
		}
	}
}

func TestTerminalStepsInTheHistoryNote(t *testing.T) {
	zero, one := 0, 1
	note := webStepsNote([]conversation.ToolActivity{
		{Tool: "run_command", Command: "ls -la", ExitCode: &zero},
		{Tool: "run_script", Language: "python", Purpose: "parsing the log", ExitCode: &one},
		{Tool: "run_command", Command: "sleep 99", TimedOut: true, ExitCode: new(int)},
	})
	for _, want := range []string{"used your tools", "- ran the command `ls -la` (exit code 0)", "- wrote and ran a python script for parsing the log (exit code 1)", "(killed at the time limit)"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
}

func TestStoppedTerminalStep(t *testing.T) {
	zero := 0
	got := finishedSteps([]conversation.ToolActivity{
		{Tool: "run_command", Command: "sleep 9", Pending: true, Phase: "running"},
		{Tool: "run_command", Command: "true", ExitCode: &zero},
	})
	if got[0].Error != "stopped" || got[0].Pending || got[0].Phase != "" || got[1].Error != "" {
		t.Fatalf("steps = %+v", got)
	}
}

func TestBoundActivityKeepsTerminalSteps(t *testing.T) {
	code := 2
	got := boundActivity([]conversation.ToolActivity{{Tool: "run_script", Language: "ruby", Purpose: "p", Command: strings.Repeat("x", 5000), Output: "secret output", ExitCode: &code}})
	if len(got) != 1 || got[0].Language != "" || got[0].Output != "" || len([]rune(got[0].Command)) > 501 || *got[0].ExitCode != 2 {
		t.Fatalf("bound = %+v", got)
	}
}

func TestHeadAndTail(t *testing.T) {
	text := strings.Repeat("a", 100) + strings.Repeat("z", 100)
	got := headAndTail(text, 30)
	if !strings.HasPrefix(got, strings.Repeat("a", 20)) || !strings.HasSuffix(got, strings.Repeat("z", 10)) || !strings.Contains(got, "170 characters left out") {
		t.Fatalf("got %q", got)
	}
}

func toolCall(name, arguments string) provider.ToolCall {
	return provider.ToolCall{ID: "t1", Name: name, Arguments: arguments}
}

func toolProgress(index int, name, arguments string) provider.ToolCallProgress {
	return provider.ToolCallProgress{Index: index, Name: name, Arguments: arguments}
}

func TestNoTerminalNoteOnlyWhenThePromptDescribesTheTerminal(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		return 200, []string{textChunk("Hi."), usageChunk}
	}}
	s := newWebServer(t, vendor) // no Terminal
	s.SystemPrompts = map[string]string{"Default": "core prompt\n## The terminal\nYou have run_command and run_script."}
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if main := vendor.mainCalls(); !hasSystemMessage(main[0], noTerminalNote) {
		t.Fatal("a model without the terminal wasn't told so")
	}

	vendor2 := &scriptedPolza{reply: vendor.reply}
	s2 := newTerminalServer(t, vendor2, "*")
	s2.SystemPrompts = s.SystemPrompts
	if _, err := collectStream(s2, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if main := vendor2.mainCalls(); hasSystemMessage(main[0], noTerminalNote) || !hasSystemPrefix(main[0], "You have a terminal") {
		t.Fatal("a model with the terminal got the wrong note")
	}
}

// The terminal section is part of the core prompt every persona shares.
func TestPersonaPromptsDescribeTheTerminal(t *testing.T) {
	prompts := LoadSystemPrompts("../prompts")
	var core string
	for _, name := range SystemPromptNames {
		text := prompts[name]
		at := strings.Index(text, "## The terminal")
		if at < 0 {
			t.Fatalf("%s has no terminal section", name)
		}
		section := text[at:]
		if core == "" {
			core = section
		} else if section != core {
			t.Fatalf("%s's terminal section differs from the others", name)
		}
		if !promptDescribesTerminal([]provider.Message{{Role: "system", Content: text}}) {
			t.Fatalf("%s isn't recognised as describing the terminal", name)
		}
	}
	for _, want := range []string{"run_script", "send_file", "sandbox:", "Writing a script for", "Never claim you ran something", "ask first"} {
		if !strings.Contains(core, want) {
			t.Errorf("terminal section lacks %q", want)
		}
	}
}

func TestSendFileGoesWithTheAnswer(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		switch len(toolMessages(body)) {
		case 0:
			return 200, []string{toolCallChunk([3]string{"c1", "run_command", `{"command":"printf '<!doctype html><title>Dash</title>' > dash_board.html"}`}), usageChunk}
		case 1:
			return 200, []string{toolCallChunk([3]string{"c2", "send_file", `{"path":"dash_board.html"}`}), usageChunk}
		}
		return 200, []string{textChunk("Here it is."), usageChunk}
	}}
	s := newTerminalServer(t, vendor, "*")
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "make a page", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if len(done.Files) != 1 || done.Files[0].Name != "dash_board.html" || done.Files[0].Kind != "text" || done.Files[0].MIME != "text/html" {
		t.Fatalf("files = %+v", done.Files)
	}
	stored, err := s.Attachments.Get(context.Background(), "u1", done.Files[0].ID)
	if err != nil || !strings.Contains(string(stored.Data), "<title>Dash</title>") || stored.ConversationID != done.ConversationID {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	step := done.Activity[1]
	if step.Tool != "send_file" || step.Title != "dash_board.html" || step.Size == 0 {
		t.Fatalf("step = %+v", step)
	}
	if got := toolMessages(vendor.mainCalls()[2])[1]["content"].(string); !strings.HasPrefix(got, "Sent dash_board.html") {
		t.Fatalf("tool result = %q", got)
	}
}

func TestSandboxLinksAreRescued(t *testing.T) {
	s := newTerminalServer(t, &scriptedPolza{reply: func(map[string]any) (int, []string) { return 200, nil }}, "*")
	r := &webRun{s: s, req: chatRequest{UserID: "u1"}, prepared: preparedRequest{conversationID: "c1"}}
	dir, _ := s.Terminal.Dir("u1", "c1")
	if err := os.WriteFile(filepath.Join(dir, "sales_dashboard_artifact.html"), []byte("<!doctype html><p>hi</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	text := "Done.\n\n[Open the dashboard](sandbox:" + dir + "/sales_dashboard_artifact.html)\n\nAlso [missing](sandbox:/tmp/nope.txt) and file://" + dir + "/sales_dashboard_artifact.html."
	got := r.rescueFileLinks(context.Background(), text)
	if strings.Contains(got, "sandbox:") || strings.Contains(got, "file://") {
		t.Fatalf("links left: %s", got)
	}
	if !strings.Contains(got, "Open the dashboard (**sales_dashboard_artifact.html**, below)") || !strings.Contains(got, "Also missing and") {
		t.Fatalf("got %s", got)
	}
	if files := r.sentFiles(); len(files) != 1 {
		t.Fatalf("files = %+v (the same file twice is one file)", files)
	}
}

func TestSendFileRefusesWhatItCantSend(t *testing.T) {
	s := newTerminalServer(t, &scriptedPolza{reply: func(map[string]any) (int, []string) { return 200, nil }}, "*")
	r := &webRun{s: s, req: chatRequest{UserID: "u1"}, prepared: preparedRequest{conversationID: "c1"}}
	dir, _ := s.Terminal.Dir("u1", "c1")
	_ = os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o600)
	for _, path := range []string{"nope.txt", ".", "empty.txt"} {
		if _, err := r.sendFile(context.Background(), path, ""); err == nil {
			t.Errorf("%s: want an error", path)
		}
	}
}
