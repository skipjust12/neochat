package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"neochat/provider"
)

func inlineImageVendor() *scriptedPolza {
	return &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		switch len(toolMessages(body)) {
		case 0:
			return 200, []string{textChunk("Before the diagram."), toolCallChunk([3]string{"make", "run_command", `{"command":"printf '` + base64.StdEncoding.EncodeToString(testPNG) + `' | base64 -d > diagram.png"}`}), usageChunk}
		case 1:
			return 200, []string{toolCallChunk([3]string{"show", "show_image", `{"path":"diagram.png","caption":"Схема: вход → выход"}`}), usageChunk}
		}
		return 200, []string{textChunk("After the diagram."), usageChunk}
	}}
}

func inlineImageRequest() chatRequest {
	return chatRequest{UserID: "u1", PlanID: "pro", Message: "Explain and draw a diagram", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}
}

func TestShowImageStreamsBetweenParagraphsAndPersists(t *testing.T) {
	vendor := inlineImageVendor()
	s := newTerminalServer(t, vendor, "*")
	events, err := collectStream(s, context.Background(), inlineImageRequest())
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	text := done.ResponseText
	before, shown, after := strings.Index(text, "Before the diagram."), strings.Index(text, "<!--neochat-image:"), strings.Index(text, "After the diagram.")
	if before < 0 || shown <= before || after <= shown {
		t.Fatalf("wrong image position: %q", text)
	}
	var deltas strings.Builder
	for _, event := range events["delta"] {
		deltas.WriteString(event.(map[string]string)["text"])
	}
	if deltas.String() != text {
		t.Fatalf("stream %q differs from saved answer %q", deltas.String(), text)
	}
	refs := inlineImageFiles(text)
	if len(refs) != 1 || len(done.Files) != 1 || refs[0].ID != done.Files[0].ID || refs[0].Width != 8 || refs[0].Height != 8 {
		t.Fatalf("files: %+v, %+v", refs, done.Files)
	}
	file, err := s.Attachments.Get(context.Background(), "u1", refs[0].ID)
	if err != nil || file.ConversationID != done.ConversationID {
		t.Fatalf("file not claimed: %+v %v", file, err)
	}
	if _, err := s.Attachments.Get(context.Background(), "u2", refs[0].ID); err == nil {
		t.Fatal("another user can read the image")
	}
	history, err := s.Conversations.History(context.Background(), "u1", done.ConversationID, 0)
	if err != nil || len(history) != 2 || history[1].Content != text || len(history[1].Versions[0].Files) != 1 {
		t.Fatalf("history: %+v %v", history, err)
	}
	note := assistantHistoryContent(history[1])
	if strings.Contains(note, "<!--neochat-image:") || !strings.Contains(note, "Схема: вход → выход") {
		t.Fatalf("model history: %q", note)
	}
	if len(vendor.mainCalls()) != 3 {
		t.Fatal("show_image ended the turn instead of continuing it")
	}
}

func TestShowImageStopKeepsTheImage(t *testing.T) {
	s := newTerminalServer(t, inlineImageVendor(), "*")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var content strings.Builder
	var cid string
	err := s.handleStream(ctx, inlineImageRequest(), s.Plans["pro"], func(event string, payload any) {
		if event == "meta" {
			cid = payload.(map[string]any)["conversation_id"].(string)
		}
		if event == "delta" {
			text := payload.(map[string]string)["text"]
			content.WriteString(text)
			if strings.Contains(text, "<!--neochat-image:") {
				cancel()
			}
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: %v", err)
	}
	history, err := s.Conversations.History(context.Background(), "u1", cid, 0)
	if err != nil || len(history) != 2 || history[1].Content != content.String() {
		t.Fatalf("stopped history: %+v %v", history, err)
	}
	refs := history[1].Versions[0].Files
	if len(refs) != 1 {
		t.Fatalf("stopped image lost: %+v", history[1])
	}
	file, err := s.Attachments.Get(context.Background(), "u1", refs[0].ID)
	if err != nil || file.ConversationID != cid {
		t.Fatalf("stopped image unclaimed: %+v %v", file, err)
	}
}

func TestShowImageRejectsNonImagesWithoutSendingAFile(t *testing.T) {
	s := newTerminalServer(t, inlineImageVendor(), "*")
	r := &webRun{s: s, req: inlineImageRequest(), prepared: preparedRequest{conversationID: "c1"}, target: executeOnServer}
	dir, _ := s.Terminal.Dir("u1", "c1")
	for _, data := range [][]byte{[]byte("this is not a PNG"), []byte("<svg><script>alert(1)</script></svg>"), testPNG[:16]} {
		if err := os.WriteFile(filepath.Join(dir, "fake.png"), data, 0600); err != nil {
			t.Fatal(err)
		}
		result, job := r.planShowImage(toolCall("show_image", `{"path":"fake.png","caption":"test"}`), 0)
		if job == nil {
			t.Fatalf("plan: %s", result)
		}
		if result = job(context.Background()); !strings.HasPrefix(result, "Error:") {
			t.Fatalf("accepted invalid image: %s", result)
		}
		if len(r.sentFiles()) != 0 || len(r.takeInlineImages()) != 0 {
			t.Fatal("invalid image was sent as a file")
		}
	}
}

func TestShowImageNonStreamingAndParallelOrder(t *testing.T) {
	s := newTerminalServer(t, inlineImageVendor(), "*")
	r := &webRun{s: s, req: inlineImageRequest(), prepared: preparedRequest{conversationID: "c1"}, target: executeOnServer}
	dir, _ := s.Terminal.Dir("u1", "c1")
	for _, name := range []string{"a.png", "b.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), testPNG, 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	r.generate = func(ctx context.Context, messages []provider.Message) (provider.GenerateResult, error) {
		calls++
		if calls == 1 {
			return provider.GenerateResult{Text: "Before.", ToolCalls: []provider.ToolCall{toolCall("show_image", `{"path":"a.png","caption":"First"}`), toolCall("show_image", `{"path":"b.png","caption":"Second"}`)}}, nil
		}
		return provider.GenerateResult{Text: "After."}, nil
	}
	answer, err := r.loop(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var captions []string
	for _, match := range inlineImagePattern.FindAllStringSubmatch(answer.Text, -1) {
		data, _ := base64.RawURLEncoding.DecodeString(match[1])
		var shown inlineImage
		if err := json.Unmarshal(data, &shown); err != nil {
			t.Fatal(err)
		}
		captions = append(captions, shown.Caption)
	}
	if strings.Join(captions, ",") != "First,Second" || !strings.HasPrefix(answer.Text, "Before.") || !strings.HasSuffix(answer.Text, "After.") {
		t.Fatalf("wrong order: %s", answer.Text)
	}
}

func TestShowImageFromComputer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-diagram.png")
	if err := os.WriteFile(path, testPNG, 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": path, "caption": "Local image"})
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if len(toolMessages(body)) == 0 {
			return 200, []string{textChunk("Before."), toolCallChunk([3]string{"show", "show_image", string(args)}), usageChunk}
		}
		return 200, []string{textChunk("After."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	connectComputer(t, s)
	req := inlineImageRequest()
	req.ExecuteOn = executeOnComputer
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if len(done.Files) != 1 || len(inlineImageFiles(done.ResponseText)) != 1 || !strings.Contains(done.ResponseText, "After.") {
		t.Fatalf("computer image: %+v", done)
	}
}
