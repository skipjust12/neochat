package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func nativeFixture(t *testing.T, handler http.HandlerFunc) *PoeClient {
	c := poeFixture(t, handler)
	c.BotQuery = true
	c.BotBaseURL = c.BaseURL
	c.UploadURL = c.BaseURL + "/upload"
	return c
}
func TestBotQueryToolsAndContinuation(t *testing.T) {
	calls := 0
	c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/Claude-Fable-5.2" || r.Header.Get("Authorization") != "Bearer user-key" {
			t.Errorf("request: %s %v", r.URL.Path, r.Header)
		}
		body := readPoeBody(t, r)
		if body["type"] != "query" || body["version"] != "1.2" || len(body["tools"].([]any)) != 1 {
			t.Errorf("payload: %+v", body)
		}
		query := body["query"].([]any)
		if query[0].(map[string]any)["role"] != "system" {
			t.Error("lost core prompt")
		}
		if calls == 1 {
			fmt.Fprint(w, "event: json\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"type\":\"function\",\"function\":{\"name\":\"run_script\",\"arguments\":\"{\\\"code\\\":\\\"print(1)\\\"}\"}}]}}]}\n\nevent: done\ndata: {}\n\n")
			return
		}
		results := body["tool_results"].([]any)
		result := results[0].(map[string]any)
		if result["tool_call_id"] != "call1" || result["name"] != "run_script" || result["content"] != "1" {
			t.Errorf("results: %v", results)
		}
		fmt.Fprint(w, "event: text\ndata: {\"text\":\"Done\"}\n\nevent: done\ndata: {}\n\n")
	})
	ctx := WithFunctionTools(WithAPIKey(context.Background(), "user-key"), []FunctionTool{{Name: "run_script", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}})
	messages := []Message{{Role: "system", Content: "core"}, {Role: "user", Content: "run it"}}
	result, err := c.Generate(ctx, "Claude-Fable-5.2", messages)
	if err != nil || len(result.ToolCalls) != 1 {
		t.Fatalf("tools: %+v %v", result, err)
	}
	messages = append(messages, Message{Role: "assistant", ToolCalls: result.ToolCalls}, Message{Role: "tool", ToolCallID: "call1", Content: "1"})
	result, err = c.Generate(ctx, "Claude-Fable-5.2", messages)
	if err != nil || result.Text != "Done" || !result.UsageEstimated || result.CachedTokens != 0 {
		t.Fatalf("continuation: %+v %v", result, err)
	}
}
func TestBotQueryUploadFilesResetAndParameters(t *testing.T) {
	uploads := 0
	c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upload" {
			uploads++
			if r.Header.Get("Authorization") != "user-key" {
				t.Error("upload auth")
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			if header.Filename != "image.png" {
				t.Error(header.Filename)
			}
			fmt.Fprint(w, `{"attachment_url":"https://cdn.example/image.png","mime_type":"image/png"}`)
			return
		}
		body := readPoeBody(t, r)
		turn := body["query"].([]any)[0].(map[string]any)
		if len(turn["attachments"].([]any)) != 1 || turn["parameters"].(map[string]any)["aspect_ratio"] != "16:9" {
			t.Errorf("turn: %v", turn)
		}
		fmt.Fprint(w, "event: text\ndata: {\"text\":\"Working\"}\n\nevent: replace_response\ndata: {\"text\":\"Finished\"}\n\nevent: file\ndata: {\"url\":\"https://cdn.example/result.png\",\"content_type\":\"image/png\",\"name\":\"result.png\"}\n\nevent: done\ndata: {}\n\n")
	})
	ctx := WithPoeBotParameters(WithAPIKey(context.Background(), "user-key"), map[string]any{"aspect_ratio": "16:9"})
	ch, err := c.GenerateStream(ctx, "Nano-Banana-2.1", []Message{{Role: "user", Content: "edit", Parts: []Part{{Type: PartImage, Name: "image.png", MIME: "image/png", Data: []byte("png")}}}})
	if err != nil {
		t.Fatal(err)
	}
	reset := false
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if chunk.Reset {
			reset = true
		}
		if chunk.Done && (chunk.Final.Text != "Finished" || len(chunk.Final.Attachments) != 1) {
			t.Errorf("result %+v", chunk.Final)
		}
	}
	if !reset || uploads != 1 {
		t.Fatalf("reset %v uploads %d", reset, uploads)
	}
}
func TestBotQueryRequiresDoneAndDoesNotReplayAcceptedError(t *testing.T) {
	for _, payload := range []string{"event: text\ndata: {\"text\":\"Partial\"}\n\n", "event: error\ndata: {\"text\":\"Unavailable\",\"allow_retry\":true}\n\n"} {
		calls := 0
		c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, payload) })
		_, err := c.Generate(context.Background(), "GPT-6.1-Sol", []Message{{Role: "user", Content: "hi"}})
		if err == nil || calls != 1 {
			t.Fatalf("accepted error: calls=%d err=%v", calls, err)
		}
	}
}
func TestBotQueryDoneWithoutData(t *testing.T) {
	c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "event: text\ndata: {\"text\":\"OK\"}\n\nevent: done\n\n")
	})
	result, err := c.Generate(context.Background(), "GPT-6-Luna", []Message{{Role: "user", Content: "hi"}})
	if err != nil || !strings.Contains(result.Text, "OK") {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestBotQueryUploadsAreCachedOnlyWithinRequestAndKey(t *testing.T) {
	uploads := 0
	c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		uploads++
		fmt.Fprint(w, `{"attachment_url":"https://cdn.example/file","mime_type":"image/png"}`)
	})
	p := Part{Type: PartImage, Name: "file.png", MIME: "image/png", Data: []byte("file")}
	ctx := WithPoeRequest(WithAPIKey(context.Background(), "first"))
	for i := 0; i < 2; i++ {
		if _, err := c.cachedBotPart(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if uploads != 1 {
		t.Fatalf("same request uploaded %d times", uploads)
	}
	if _, err := c.cachedBotPart(WithAPIKey(ctx, "second"), p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.cachedBotPart(WithPoeRequest(WithAPIKey(context.Background(), "first")), p); err != nil {
		t.Fatal(err)
	}
	if uploads != 3 {
		t.Fatalf("cross-key/request cache reuse: %d", uploads)
	}
}
