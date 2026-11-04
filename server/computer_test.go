package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"neochat/auth"
	"neochat/conversation"
	"neochat/device"
	"neochat/terminal"
)

// connectComputer starts a device.Client against s (served by an
// httptest server) for user u1, and waits until the server sees it.
func connectComputer(t *testing.T, s *Server) (string, *httptest.Server) {
	t.Helper()
	store := s.Auth.(*auth.InMemoryStore)
	key, err := store.IssueKey(context.Background(), "u1", "pro")
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s.Mux())
	t.Cleanup(web.Close)
	work := t.TempDir()
	client := &device.Client{
		Server: web.URL, Key: key, Version: "test",
		Executor: &device.Executor{Runner: &terminal.Runner{WorkDir: work, InheritEnv: true, Users: []string{"*"}}, WorkDir: work},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = client.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for s.devices().device("u1") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the computer never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return work, web
}

func TestComputerRunsCommandsAfterApproval(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		switch len(toolMessages(body)) {
		case 0:
			return 200, []string{toolCallChunk([3]string{"c1", "write_file", `{"path":"notes/hello.txt","content":"hi from the model"}`}), usageChunk}
		case 1:
			return 200, []string{toolCallChunk([3]string{"c2", "run_command", `{"command":"cat notes/hello.txt","shell":"bash"}`}), usageChunk}
		}
		return 200, []string{textChunk("Done."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	work, _ := connectComputer(t, s)

	// Approve everything that asks, from "outside", as the UI would.
	approved := map[string]bool{}
	var answered []string
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "write a note", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", ExecuteOn: executeOnComputer, Approval: approvalAsk}
	events := map[string][]any{}
	err := s.handleStream(context.Background(), req, s.Plans["pro"], func(event string, payload any) {
		events[event] = append(events[event], payload)
		if event != "activity" {
			return
		}
		for _, step := range payload.(map[string]any)["activity"].([]conversation.ToolActivity) {
			if step.Approval == "pending" && step.ApprovalID != "" && !approved[step.ApprovalID] {
				approved[step.ApprovalID] = true
				answered = append(answered, step.Tool)
				id := step.ApprovalID
				go s.approvals().answer("u1", id, true)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if len(done.Activity) != 2 {
		t.Fatalf("activity = %+v", done.Activity)
	}
	write, run := done.Activity[0], done.Activity[1]
	if write.Tool != "write_file" || write.Where != "computer" || write.Approval != "allowed" || write.Error != "" {
		t.Fatalf("write = %+v", write)
	}
	if run.Tool != "run_command" || run.Output != "hi from the model" || run.Approval != "allowed" || run.Shell != "bash" {
		t.Fatalf("run = %+v", run)
	}
	if strings.Join(answered, ",") != "write_file,run_command" {
		t.Fatalf("approvals asked for %v", answered)
	}
	// The file is in the chat's working directory on the "computer".
	matches, _ := filepath.Glob(filepath.Join(work, "*", "notes", "hello.txt"))
	if len(matches) != 1 {
		t.Fatalf("file not written under %s", work)
	}

	main := vendor.mainCalls()
	names := strings.Join(toolNames(main[0]), ",")
	for _, want := range []string{"run_command", "run_script", "read_file", "write_file", "list_dir", "send_file"} {
		if !strings.Contains(names, want) {
			t.Fatalf("tools = %s", names)
		}
	}
	if !hasSystemPrefix(main[0], "Your tools run on the user's own computer") {
		t.Fatal("no computer note")
	}
}

func TestComputerDeniedActionDoesNotRun(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if len(toolMessages(body)) == 0 {
			return 200, []string{toolCallChunk([3]string{"c1", "run_command", `{"command":"touch should-not-exist"}`}), usageChunk}
		}
		return 200, []string{textChunk("Okay."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	work, _ := connectComputer(t, s)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "go", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", ExecuteOn: executeOnComputer, Approval: approvalAsk}
	events := map[string][]any{}
	err := s.handleStream(context.Background(), req, s.Plans["pro"], func(event string, payload any) {
		events[event] = append(events[event], payload)
		if event == "activity" {
			for _, step := range payload.(map[string]any)["activity"].([]conversation.ToolActivity) {
				if step.ApprovalID != "" {
					go s.approvals().answer("u1", step.ApprovalID, false)
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	step := events["done"][0].(chatResponse).Activity[0]
	if step.Error != "denied" || step.Approval != "denied" || step.ExitCode != nil {
		t.Fatalf("step = %+v", step)
	}
	if matches, _ := filepath.Glob(filepath.Join(work, "*", "should-not-exist")); len(matches) != 0 {
		t.Fatal("a denied command ran")
	}
	if got := toolMessages(vendor.mainCalls()[1])[0]["content"].(string); got != deniedResult {
		t.Fatalf("tool result = %q", got)
	}
}

func TestComputerNotConnected(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		return 200, []string{textChunk("Open the app."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "ls", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", ExecuteOn: executeOnComputer}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	main := vendor.mainCalls()[0]
	if !hasSystemMessage(main, computerMissingNote) {
		t.Fatal("not told the computer isn't connected")
	}
	for _, name := range toolNames(main) {
		if isTerminalTool(name) {
			t.Fatalf("tools = %v", toolNames(main))
		}
	}
}

func TestComputerSendsFiles(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if len(toolMessages(body)) == 0 {
			return 200, []string{toolCallChunk([3]string{"c1", "send_file", `{"path":"~/report.csv"}`}), usageChunk}
		}
		return 200, []string{textChunk("Sent."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "report.csv"), []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connectComputer(t, s)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "send it", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", ExecuteOn: executeOnComputer, Approval: approvalAllow}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if len(done.Files) != 1 || done.Files[0].Name != "report.csv" {
		t.Fatalf("files = %+v", done.Files)
	}
}

func TestDeviceEndpointsNeedAKey(t *testing.T) {
	s := newWebServer(t, &scriptedPolza{reply: func(map[string]any) (int, []string) { return 200, nil }})
	key, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u1", "pro")
	web := httptest.NewServer(s.Mux())
	defer web.Close()

	resp, err := http.Get(web.URL + "/device/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without a key = %d", resp.StatusCode)
	}
	r, _ := http.NewRequest(http.MethodGet, web.URL+"/device/status", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var status struct{ Connected bool }
	_ = json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	if resp.StatusCode != 200 || status.Connected {
		t.Fatalf("status = %d %+v", resp.StatusCode, status)
	}

	// Approving someone else's action, or none, is a 404.
	id, _ := s.approvals().open("someone-else")
	body, _ := json.Marshal(map[string]any{"id": id, "allow": true})
	r, _ = http.NewRequest(http.MethodPost, web.URL+"/chat/approve", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("approve someone else's = %d", resp.StatusCode)
	}
}

func TestMCPToolNames(t *testing.T) {
	names := mcpToolNames([]device.Server{{Name: "blender", Tools: []device.Tool{{Name: "get_scene_info"}, {Name: "execute blender code"}}}, {Name: strings.Repeat("x", 70), Tools: []device.Tool{{Name: "a"}, {Name: "b"}}}})
	if names["mcp__blender__get_scene_info"] != [2]string{"blender", "get_scene_info"} || names["mcp__blender__execute_blender_code"][1] != "execute blender code" {
		t.Fatalf("names = %v", names)
	}
	if len(names) != 4 {
		t.Fatalf("long names collided: %v", names)
	}
	for name := range names {
		if len(name) > 64 {
			t.Fatalf("%s is too long", name)
		}
	}
}
