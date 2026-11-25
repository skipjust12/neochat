package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"neochat/provider"
	"neochat/router"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestViewImageInspectFixThenShow(t *testing.T) {
	var buf bytes.Buffer
	red := image.NewRGBA(image.Rect(0, 0, 8, 8))
	red.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&buf, red); err != nil {
		t.Fatal(err)
	}
	fixed := buf.Bytes()
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		n := len(toolMessages(body))
		if n == 1 || n == 3 {
			var seen string
			for _, turn := range body["messages"].([]any) {
				m := turn.(map[string]any)
				if m["role"] != "user" {
					continue
				}
				parts, ok := m["content"].([]any)
				if !ok {
					continue
				}
				for _, p := range parts {
					part := p.(map[string]any)
					if part["type"] == "image_url" {
						seen = part["image_url"].(map[string]any)["url"].(string)
					}
				}
			}
			expected := testPNG
			if n == 3 {
				expected = fixed
			}
			if seen != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(expected) {
				t.Errorf("round %d did not deliver the current image to the model", n)
			}
		}
		switch n {
		case 0:
			return 200, []string{toolCallChunk([3]string{"view1", "view_image", `{"path":"diagram.png"}`}), usageChunk}
		case 1:
			return 200, []string{toolCallChunk([3]string{"fix", "run_command", `{"command":"printf '` + base64.StdEncoding.EncodeToString(fixed) + `' | base64 -d > diagram.png"}`}), usageChunk}
		case 2:
			return 200, []string{toolCallChunk([3]string{"view2", "view_image", `{"path":"diagram.png"}`}), usageChunk}
		case 3:
			return 200, []string{toolCallChunk([3]string{"show", "show_image", `{"path":"diagram.png","caption":"Checked diagram"}`}), usageChunk}
		}
		return 200, []string{textChunk("Checked and corrected."), usageChunk}
	}}
	s := newTerminalServer(t, vendor, "*")
	req := inlineImageRequest()
	req.ConversationID = "visual-check"
	dir, _ := s.Terminal.Dir("u1", req.ConversationID)
	if err := os.WriteFile(filepath.Join(dir, "diagram.png"), testPNG, 0600); err != nil {
		t.Fatal(err)
	}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if len(done.Files) != 1 || strings.Count(done.ResponseText, "<!--neochat-image:") != 1 {
		t.Fatalf("only show_image may publish: %+v", done)
	}
	var views int
	for _, a := range done.Activity {
		if a.Tool == "view_image" {
			views++
			if a.Error != "" {
				t.Error(a.Error)
			}
		}
	}
	if views != 2 {
		t.Fatalf("view steps=%d", views)
	}
	file, err := s.Attachments.Get(context.Background(), "u1", done.Files[0].ID)
	if err != nil || !bytes.Equal(file.Data, fixed) {
		t.Fatal("published image was not the corrected one", err)
	}
}

func TestViewImagePrivateParallelAndValidation(t *testing.T) {
	s := newTerminalServer(t, &scriptedPolza{}, "*")
	s.Attachments = nil
	r := webRun{s: s, req: inlineImageRequest(), prepared: preparedRequest{conversationID: "c"}, target: executeOnServer, model: router.Model{InputModalities: []string{"text", "image"}}}
	dir, _ := s.Terminal.Dir("u1", "c")
	if err := os.WriteFile(filepath.Join(dir, "a.png"), testPNG, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "text.txt"), []byte("not a picture"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := []provider.ToolCall{{ID: "a", Name: "view_image", Arguments: `{"path":"a.png"}`}, {ID: "b", Name: "view_image", Arguments: `{"path":"a.png"}`}, {ID: "bad", Name: "view_image", Arguments: `{"path":"text.txt"}`}, {ID: "missing", Name: "view_image", Arguments: `{"path":"missing.png"}`}}
	results := r.runTools(context.Background(), calls, nil)
	pictures := r.takeViewedImages()
	if len(pictures) != 2 || !bytes.Equal(pictures[0].Data, testPNG) || !bytes.Equal(pictures[1].Data, testPNG) || !strings.HasPrefix(results[2], "Error:") || !strings.HasPrefix(results[3], "Error:") {
		t.Fatalf("pictures=%v results=%v", len(pictures), results)
	}
	if len(r.files) != 0 || len(r.inlineImages) != 0 {
		t.Fatal("view_image published an attachment")
	}
	r.model.InputModalities = []string{"text"}
	for _, tool := range r.terminalTools() {
		if tool.Name == "view_image" {
			t.Fatal("text-only model got view_image")
		}
	}
	if result, job := r.planViewImage(calls[0], 0); job != nil || !strings.HasPrefix(result, "Error:") {
		t.Fatal("text-only view allowed")
	}
	r.model.InputModalities = []string{"image"}
	r.views = 10
	if _, job := r.planViewImage(calls[0], 0); job != nil {
		t.Fatal("view limit bypassed")
	}
}

func TestViewImageOnConnectedComputer(t *testing.T) {
	s := newTerminalServer(t, &scriptedPolza{}, "*")
	work, _ := connectComputer(t, s)
	path := filepath.Join(work, "local-image.png")
	if err := os.WriteFile(path, testPNG, 0600); err != nil {
		t.Fatal(err)
	}
	r := webRun{s: s, req: inlineImageRequest(), prepared: preparedRequest{conversationID: "c"}, target: executeOnComputer, dev: s.devices().device("u1"), model: router.Model{InputModalities: []string{"image"}}}
	result, job := r.planViewImage(provider.ToolCall{Name: "view_image", Arguments: `{"path":"` + path + `"}`}, 0)
	if job == nil {
		t.Fatal(result)
	}
	if result = job(context.Background()); strings.HasPrefix(result, "Error:") {
		t.Fatal(result)
	}
	if !bytes.Equal(r.takeViewedImages()[0].Data, testPNG) || len(r.files) != 0 {
		t.Fatal("computer image was not privately delivered")
	}
}
