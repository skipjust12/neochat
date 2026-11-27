package provider

import (
	"context"
	"encoding/json"
	"errors"
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

func TestBotQueryAccessDenialAndExplicitFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/Official" {
					fmt.Fprint(w, "event: error\ndata: {\"text\":\"This bot does not support API access\"}\n\n")
					return
				}
				if r.URL.Path != "/MyPublicBot" {
					t.Error(r.URL.Path)
				}
				fmt.Fprint(w, "event: text\ndata: {\"text\":\"OK\"}\n\nevent: done\ndata: {}\n\n")
			})
			if stream {
				if _, err := c.GenerateStream(context.Background(), "Official", []Message{{Role: "user", Content: "hi"}}); err == nil {
					t.Fatal("denial hidden")
				}
			} else {
				if _, err := c.Generate(context.Background(), "Official", []Message{{Role: "user", Content: "hi"}}); err == nil {
					t.Fatal("denial hidden")
				}
			}
			c.BotFallbacks = map[string]string{"official": "MyPublicBot"}
			if stream {
				ch, err := c.GenerateStream(context.Background(), "Official", []Message{{Role: "user", Content: "hi"}})
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range ch {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				result, err := c.Generate(context.Background(), "Official", []Message{{Role: "user", Content: "hi"}})
				if err != nil || result.Text != "OK" {
					t.Fatalf("%+v %v", result, err)
				}
			}
			if calls != 3 {
				t.Fatalf("calls %d", calls)
			}
		})
	}
}

func TestBotQueryFormattedThinkingUsesReasoningChannel(t *testing.T) {
	c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "event: text\ndata: {\"text\":\"*Thinking...*\\n\\n> Private thought.\\n\\nVisible answer.\"}\n\nevent: done\ndata: {}\n\n")
	})
	ch, err := c.GenerateStream(context.Background(), "Claude-Opus-5.5", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	answer, thought := "", ""
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		answer += chunk.Delta
		thought += chunk.Reasoning
		if chunk.Done && (strings.Contains(chunk.Final.Text, "Private") || !strings.Contains(chunk.Final.Reasoning, "Private thought.")) {
			t.Fatalf("final %+v", chunk.Final)
		}
	}
	if strings.Contains(answer, "Private") || strings.Contains(answer, "Thinking") || !strings.Contains(answer, "Visible answer.") || !strings.Contains(thought, "Private thought.") {
		t.Fatalf("answer %q thought %q", answer, thought)
	}
}

func TestBotQueryMultipleToolRoundsPreserveTypedHistoryAndProse(t *testing.T) {
	c := NewPoeClient("key")
	messages := []Message{{Role: "system", Content: "core: retain exact facts"}, {Role: "user", Content: "Never guess; required total is 12345."},
		{Role: "assistant", Content: "First step", ToolCalls: []ToolCall{{ID: "first", Name: "web_fetch", Arguments: `{"url":"https://example.com"}`}}},
		{Role: "tool", ToolCallID: "first", Content: "Verified report: 12345, not 12354."},
		{Role: "assistant", Content: "Second step", ToolCalls: []ToolCall{{ID: "second", Name: "run_script", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "second", Content: "output"}}
	body, err := c.botRequest(context.Background(), "bot", messages)
	if err != nil {
		t.Fatal(err)
	}
	query := body["query"].([]any)
	roles := []string{}
	for _, raw := range query {
		roles = append(roles, raw.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,bot,bot,tool,bot" {
		t.Fatalf("wrong historical roles: %v", roles)
	}
	if query[0].(map[string]any)["content"] != messages[0].Content || query[1].(map[string]any)["content"] != messages[1].Content || query[2].(map[string]any)["content"] != "First step" || query[5].(map[string]any)["content"] != "Second step" {
		t.Fatal("lost core, original instruction or intermediate text")
	}
	callTurn := query[3].(map[string]any)
	if callTurn["message_type"] != "function_call" {
		t.Fatal("historical calls lack protocol type")
	}
	var calls []map[string]any
	if err := json.Unmarshal([]byte(callTurn["content"].(string)), &calls); err != nil || len(calls) != 1 || calls[0]["id"] != "first" {
		t.Fatal("invalid call history", err)
	}
	var historical []map[string]any
	if err := json.Unmarshal([]byte(query[4].(map[string]any)["content"].(string)), &historical); err != nil || len(historical) != 1 || historical[0]["tool_call_id"] != "first" || historical[0]["content"] != messages[3].Content {
		t.Fatal("lost structured tool evidence", err)
	}
	results := body["tool_results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["tool_call_id"] != "second" {
		t.Fatalf("latest results %v", results)
	}
	if query[1].(map[string]any)["parameters"] == nil || query[5].(map[string]any)["parameters"] != nil {
		t.Fatal("parameters moved from user request to assistant prose")
	}
}

func TestBotQueryMultipartTextAndLongConversationStayIntact(t *testing.T) {
	c := NewPoeClient("key")
	messages := []Message{{Role: "system", Content: "original instructions"}}
	for i := 0; i < 40; i++ {
		messages = append(messages, Message{Role: "user", Content: fmt.Sprintf("fact %d", i)}, Message{Role: "assistant", Content: fmt.Sprintf("acknowledged %d", i)})
	}
	messages = append(messages, Message{Role: "user", Content: "stale text view", Parts: []Part{{Type: PartText, Text: "file contents"}, {Type: PartText, Text: "quotation"}, {Type: PartText, Text: "actual question"}}})
	body, err := c.botRequest(context.Background(), "bot", messages)
	if err != nil {
		t.Fatal(err)
	}
	query := body["query"].([]any)
	if len(query) != len(messages) {
		t.Fatal("trimmed short conversation")
	}
	for i := 0; i < len(messages)-1; i++ {
		if query[i].(map[string]any)["content"] != messages[i].Content {
			t.Fatalf("lost historical message %d", i)
		}
	}
	if query[len(query)-1].(map[string]any)["content"] != "file contents\n\nquotation\n\nactual question" {
		t.Fatal("lost multipart text")
	}
}

func TestBotQueryServiceFailurePreservesReferenceWithoutReplay(t *testing.T) {
	calls := 0
	c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, "event: error\ndata: {\"text\":\"Internal server error (err_id: abcdef0123456789abcdef0123456789)\",\"allow_retry\":true}\n\n")
	})
	_, err := c.Generate(context.Background(), "bot", []Message{{Role: "user", Content: "hi"}})
	var service *BotServiceError
	if !errors.As(err, &service) || service.Reference != "abcdef0123456789abcdef0123456789" || calls != 1 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}

// Inspect real HTTP request bodies across three generations, not only the
// in-memory encoder. Later steps must retain instructions, prose and typed
// evidence from every earlier tool round.
func TestBotQueryThreeGenerationsKeepEvidenceOnWire(t *testing.T) {
	calls := 0
	c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		body := readPoeBody(t, r)
		query := body["query"].([]any)
		if query[0].(map[string]any)["content"] != "Never invent verified facts." || query[1].(map[string]any)["content"] != "Use the report; required total is 12345." {
			t.Error("lost initial instruction")
		}
		known := map[string]bool{}
		userTurns := 0
		for _, raw := range query {
			m := raw.(map[string]any)
			if m["role"] == "user" {
				userTurns++
			}
			if m["message_type"] == "function_call" {
				var history []struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal([]byte(m["content"].(string)), &history); err != nil {
					t.Error(err)
				}
				for _, call := range history {
					known[call.ID] = true
				}
			}
			if m["role"] == "tool" {
				var results []struct {
					ID      string `json:"tool_call_id"`
					Content string `json:"content"`
				}
				if err := json.Unmarshal([]byte(m["content"].(string)), &results); err != nil {
					t.Error(err)
				}
				for _, result := range results {
					if !known[result.ID] || result.Content != "verified total 12345" {
						t.Error("orphaned or changed source evidence")
					}
				}
			}
		}
		if userTurns != 1 {
			t.Error("tool output became a new user request")
		}
		for step := 1; step < calls; step++ {
			found := false
			for _, raw := range query {
				if raw.(map[string]any)["content"] == fmt.Sprintf("Checked step %d.", step) {
					found = true
				}
			}
			if !found {
				t.Errorf("lost intermediate answer from step %d", step)
			}
		}
		if len(known) != max(0, calls-2) {
			t.Error("lost historical function calls")
		}
		if calls == 3 {
			fmt.Fprint(w, "event: text\ndata: {\"text\":\"Done.\"}\n\nevent: done\ndata: {}\n\n")
			return
		}
		data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprint(calls), "type": "function", "function": map[string]string{"name": "web_fetch", "arguments": "{}"}}}}}}})
		fmt.Fprintf(w, "event: json\ndata: %s\n\nevent: done\ndata: {}\n\n", data)
	})
	ctx := WithFunctionTools(context.Background(), []FunctionTool{{Name: "web_fetch", Parameters: map[string]any{"type": "object"}}})
	messages := []Message{{Role: "system", Content: "Never invent verified facts."}, {Role: "user", Content: "Use the report; required total is 12345."}}
	for step := 1; step <= 3; step++ {
		result, err := c.Generate(ctx, "model", messages)
		if err != nil {
			t.Fatal(err)
		}
		if step < 3 {
			if len(result.ToolCalls) != 1 {
				t.Fatal("missing tool call")
			}
			messages = append(messages, Message{Role: "assistant", Content: fmt.Sprintf("Checked step %d.", step), ToolCalls: result.ToolCalls}, Message{Role: "tool", ToolCallID: result.ToolCalls[0].ID, Content: "verified total 12345"})
		} else if result.Text != "Done." {
			t.Fatal("missing final output")
		}
	}
	if calls != 3 {
		t.Fatalf("unexpected extra generation: %d", calls)
	}
}

func TestBotQueryKeepsTokenLimitFinishReason(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			c := nativeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "event: text\ndata: {\"text\":\"Partial summary\"}\n\nevent: json\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\nevent: done\ndata: {}\n\n")
			})
			var result GenerateResult
			if stream {
				ch, err := c.GenerateStream(context.Background(), "bot", []Message{{Role: "user", Content: "summarize"}})
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range ch {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					if chunk.Done {
						result = chunk.Final
					}
				}
			} else {
				var err error
				result, err = c.Generate(context.Background(), "bot", []Message{{Role: "user", Content: "summarize"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if result.FinishReason != "length" || !result.Truncated() || result.Text != "Partial summary" {
				t.Fatalf("lost truncation status: %+v", result)
			}
		})
	}
}

func TestPoeIncompleteResponseWithCallsStaysTruncated(t *testing.T) {
	var response poeResponse
	if err := json.Unmarshal([]byte(`{"status":"incomplete","output":[{"type":"function_call","call_id":"c1","name":"web_fetch","arguments":"{}"}]}`), &response); err != nil {
		t.Fatal(err)
	}
	result, err := response.result()
	if err != nil || !result.Truncated() || len(result.ToolCalls) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
}
