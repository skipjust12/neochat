package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func toolImageFixture() ([]Message, []byte) {
	data := []byte("rendered-png-bytes")
	return []Message{{Role: "user", Content: "inspect"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "view", Name: "view_image", Arguments: `{"path":"plot.png"}`}, {ID: "other", Name: "run_command", Arguments: `{"command":"true"}`}}}, {Role: "tool", ToolCallID: "view", Content: "Opened plot.png", Parts: []Part{{Type: PartText, Text: "Opened plot.png"}, {Type: PartImage, Name: "plot.png", MIME: "image/png", Data: data}}}, {Role: "tool", ToolCallID: "other", Content: "done"}}, data
}

func TestToolImagesCompatibilityTransports(t *testing.T) {
	messages, data := toolImageFixture()
	ctx := context.Background()
	c := &PoeClient{}
	for name, req := range map[string]any{"chat": c.chatRequest(ctx, "model", messages, false), "responses": c.responseRequest(ctx, "model", messages, false), "claude": c.claudeRequest(ctx, "model", messages, false), "legacy": (&PolzaClient{}).buildRequest(ctx, "model", messages, false)} {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(b, []byte(base64.StdEncoding.EncodeToString(data))) || !bytes.Contains(b, []byte("Opened plot.png")) || !bytes.Contains(b, []byte("other")) {
				t.Fatalf("lost image or tool results: %s", b)
			}
		})
	}
	expanded := toolImageMessages(messages)
	if len(expanded) != 5 || expanded[2].Role != "tool" || expanded[3].ToolCallID != "other" || expanded[4].Role != "user" || len(expanded[4].Parts) != 2 {
		t.Fatalf("image interrupted parallel results: %+v", expanded)
	}
	if len(messages[2].Parts) != 2 {
		t.Fatal("mutated original context")
	}
	// Later tool rounds must retain the picture from the earlier inspection.
	messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "next", Name: "run_command", Arguments: `{}`}}}, Message{Role: "tool", ToolCallID: "next", Content: "fixed"})
	if len(toolImageMessages(messages)[4].Parts) != 2 {
		t.Fatal("lost previous round image")
	}
}

func TestBotQueryToolImageContinuationAndHistory(t *testing.T) {
	messages, data := toolImageFixture()
	uploads := 0
	c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload" {
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		uploads++
		f, header, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		got, _ := io.ReadAll(f)
		if !bytes.Equal(got, data) || header.Filename != "plot.png" {
			t.Fatal("wrong image uploaded")
		}
		fmt.Fprint(w, `{"attachment_url":"https://cdn.example/plot.png","mime_type":"image/png"}`)
	})
	ctx := WithPoeRequest(WithAPIKey(context.Background(), "key"))
	for round := 0; round < 2; round++ {
		body, err := c.botRequest(ctx, "VisionBot", messages)
		if err != nil {
			t.Fatal(err)
		}
		results := body["tool_results"].([]any)
		if len(results) != (2 - round) {
			t.Fatalf("tool results: %+v", results)
		}
		if round == 0 && results[0].(map[string]any)["tool_call_id"] != "view" {
			t.Fatal("lost image result ID")
		}
		seen := 0
		for _, turn := range body["query"].([]any) {
			m := turn.(map[string]any)
			if a, ok := m["attachments"].([]PoeAttachment); ok && len(a) > 0 {
				seen++
				if a[0].URL != "https://cdn.example/plot.png" || m["role"] != "user" {
					t.Fatalf("image turn: %+v", m)
				}
			}
		}
		if seen != 1 {
			t.Fatalf("round %d: image missing/duplicated in query: %+v", round, body)
		}
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), base64.StdEncoding.EncodeToString(data)) {
			t.Fatal("raw image in native tool text")
		}
		messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "next", Name: "run_command", Arguments: `{}`}}}, Message{Role: "tool", ToolCallID: "next", Content: "fixed"})
	}
	if uploads != 1 {
		t.Fatalf("reuploaded unchanged image across rounds: %d", uploads)
	}
}
