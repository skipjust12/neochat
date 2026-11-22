package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func poeFixture(t *testing.T, handler http.HandlerFunc) *PoeClient {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c := NewPoeClient("fallback")
	c.BaseURL = s.URL
	c.MaxTokens = 2000
	return c
}
func readPoeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		t.Error(err)
	}
	return b
}
func TestPoeChatRequestAndUserIsolation(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("anthropic-version") != "" {
			t.Errorf("wrong endpoint/headers: %s %v", r.URL.Path, r.Header)
		}
		b := readPoeBody(t, r)
		if b["model"] != "gemini" || b["max_tokens"] != float64(700) || b["thinking_level"] != "high" || b["plugins"] != nil || b["reasoning"] != nil {
			t.Errorf("request = %+v", b)
		}
		parts := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if parts[1].(map[string]any)["type"] != "image_url" || parts[2].(map[string]any)["type"] != "file" || parts[3].(map[string]any)["type"] != "file" {
			t.Errorf("multimodal parts=%+v", parts)
		}
		mu.Lock()
		seen[r.Header.Get("Authorization")] = true
		mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"content":[{"type":"text","text":"Answer"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":100}}}`)
	})
	c.ModelInfo = func(string) PoeModelInfo {
		return PoeModelInfo{ReasoningParameter: "thinking_level", ReasoningValues: []string{"low", "high"}}
	}
	var wg sync.WaitGroup
	for _, key := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			ctx := WithReasoning(WithOutputLimit(WithAPIKey(context.Background(), key), 700), Reasoning{Effort: "high"})
			out, err := c.Generate(ctx, "gemini", []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "hello"}, {Type: PartImage, MIME: "image/png", Data: []byte("png")}, {Type: PartFile, MIME: "application/pdf", Name: "a.pdf", Data: []byte("pdf")}, {Type: PartVideo, MIME: "video/mp4", Name: "a.mp4", Data: []byte("mp4")}}}})
			if err != nil || out.Text != "Answer" || out.InputTokens != 120 || out.CachedTokens != 0 || !out.Poe {
				t.Errorf("result=%+v err=%v", out, err)
			}
		}(key)
	}
	wg.Wait()
	if len(seen) != 2 || !seen["Bearer alice"] || !seen["Bearer bob"] {
		t.Fatalf("keys leaked/shared: %v", seen)
	}
}
func TestPoeChatStreamingToolsAndUsage(t *testing.T) {
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		b := readPoeBody(t, r)
		if b["stream_options"].(map[string]any)["include_usage"] != true {
			t.Error("missing usage request")
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" \\n\"}}]}\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Before ","reasoning_content":"Think"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call1","function":{"name":"run_script","arguments":"{\"code\":"}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"print(42)\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20}}`+"\n\ndata: [DONE]\n\n")
	})
	progress := []ToolCallProgress{}
	ctx := WithToolCallProgress(WithParagraphBreak(context.Background()), func(p ToolCallProgress) { progress = append(progress, p) })
	ch, err := c.GenerateStream(ctx, "bot", []Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	var text, thinking string
	var final GenerateResult
	done := 0
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		text += chunk.Delta
		thinking += chunk.Reasoning
		if chunk.Done {
			final = chunk.Final
			done++
		}
	}
	if done != 1 || text != "\n\nBefore " || final.Text != text || thinking != "Think" || final.InputTokens != 100 || len(final.ToolCalls) != 1 || final.ToolCalls[0].Arguments != `{"code":"print(42)"}` || !final.Poe || len(progress) != 2 {
		t.Fatalf("text=%q thinking=%q result=%+v progress=%v done=%d", text, thinking, final, progress, done)
	}
}
func TestPoeClaudeNativeCacheAndThinkingReplay(t *testing.T) {
	readPrice, writePrice := 0.2, 2.5
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("wrong native request: %s", r.URL.Path)
		}
		b := readPoeBody(t, r)
		system := b["system"].([]any)
		if system[len(system)-1].(map[string]any)["cache_control"] == nil {
			t.Error("system cache point missing")
		}
		if b["output_config"].(map[string]any)["effort"] != "high" || b["thinking"].(map[string]any)["type"] != "adaptive" {
			t.Error("adaptive thinking malformed")
		}
		messages := b["messages"].([]any)
		marks := 0
		for _, raw := range messages {
			for _, raw := range raw.(map[string]any)["content"].([]any) {
				if raw.(map[string]any)["cache_control"] != nil {
					marks++
				}
			}
		}
		if marks != 2 {
			t.Errorf("user cache points=%d", marks)
		}
		assistant := messages[1].(map[string]any)["content"].([]any)
		if assistant[0].(map[string]any)["signature"] != "signed" {
			t.Error("thinking signature lost")
		}
		result := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
		if result["type"] != "tool_result" || result["tool_use_id"] != "call1" {
			t.Errorf("tool result=%v", result)
		}
		fmt.Fprint(w, `{"content":[{"type":"thinking","thinking":"Thought","signature":"signature"},{"type":"text","text":"Done"}],"stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":900,"cache_creation_input_tokens":100}}`)
	})
	c.ModelInfo = func(string) PoeModelInfo {
		return PoeModelInfo{NativeMessages: true, CacheReadPrice: &readPrice, CacheWritePrice: &writePrice}
	}
	messages := []Message{{Role: "system", Content: "Stable core"}, {Role: "user", Content: "First"}, {Role: "assistant", ReasoningDetails: json.RawMessage(`[{"type":"thinking","thinking":"private","signature":"signed"}]`), ToolCalls: []ToolCall{{ID: "call1", Name: "cmd", Arguments: `{"x":1}`}}}, {Role: "tool", ToolCallID: "call1", Content: "result"}, {Role: "user", Content: "Next"}}
	out, err := c.Generate(WithReasoning(context.Background(), Reasoning{Adaptive: true, Effort: "high"}), "claude", messages)
	if err != nil || out.InputTokens != 1050 || out.CachedTokens != 900 || out.CacheWriteTokens != 100 || out.Text != "Done" || !strings.Contains(string(out.ReasoningDetails), "signature") || out.CacheReadPrice == nil {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
func TestPoeClaudeStreamingNativeUsageAndTools(t *testing.T) {
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		events := []string{`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":20}}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Thought"}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed"}}`, `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}`, `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"c1","name":"show_image","input":{}}}`, `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.png\"}"}}`, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":22}}`, `{"type":"message_stop"}`}
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	})
	c.ModelInfo = func(string) PoeModelInfo { return PoeModelInfo{NativeMessages: true} }
	ch, err := c.GenerateStream(context.Background(), "claude", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	var out GenerateResult
	var text string
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		text += chunk.Delta
		if chunk.Done {
			out = chunk.Final
		}
	}
	if text != "Hello" || out.InputTokens != 120 || out.OutputTokens != 22 || out.CachedTokens != 90 || out.CacheWriteTokens != 20 || len(out.ToolCalls) != 1 || out.ToolCalls[0].Arguments != `{"path":"a.png"}` || !out.Poe {
		t.Fatalf("native result=%+v text=%s", out, text)
	}
}
func TestPoeResponsesSearchAndEncryptedReplay(t *testing.T) {
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Error(r.URL.Path)
		}
		b := readPoeBody(t, r)
		if b["store"] != false || b["previous_response_id"] != nil || b["plugins"] != nil {
			t.Errorf("request=%v", b)
		}
		tool := b["tools"].([]any)[0].(map[string]any)
		if tool["type"] != "web_search_preview" {
			t.Error("missing Poe search")
		}
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"web_search_call","action":{"sources":[{"url":"https://example.com","title":"Example"}]}},{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Thought"}],"encrypted_content":"opaque"},{"type":"message","content":[{"type":"output_text","text":"Found","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example"}]}]}],"usage":{"input_tokens":500,"output_tokens":20,"input_tokens_details":{"cached_tokens":300}}}`)
	})
	c.ModelInfo = func(string) PoeModelInfo { return PoeModelInfo{NativeMessages: true} }
	out, err := c.Generate(WithWebPlugin(context.Background(), WebPlugin{}), "claude", []Message{{Role: "user", Content: "Search"}})
	if err != nil || out.Text != "Found" || out.WebSearches != 1 || out.CachedTokens != 300 || len(out.Citations) == 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	body := c.responseRequest(context.Background(), "bot", []Message{{Role: "assistant", ReasoningDetails: out.ReasoningDetails, ToolCalls: []ToolCall{{ID: "c1", Name: "cmd", Arguments: `{}`}}}, {Role: "tool", ToolCallID: "c1", Content: "ok"}}, false)
	input := body["input"].([]any)
	reasoning := input[0].(map[string]any)
	if reasoning["encrypted_content"] != "opaque" || reasoning["type"] != "reasoning" || reasoning["name"] != nil {
		t.Errorf("reasoning replay=%v", reasoning)
	}
}
func TestPoeStreamRejectsTruncation(t *testing.T) {
	for _, kind := range []string{"chat", "responses", "messages"} {
		t.Run(kind, func(t *testing.T) {
			c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "data: {}\n\n") })
			c.ModelInfo = func(string) PoeModelInfo {
				return PoeModelInfo{NativeMessages: kind == "messages", UseResponses: kind == "responses"}
			}
			ch, err := c.GenerateStream(context.Background(), "bot", nil)
			if err != nil {
				t.Fatal(err)
			}
			var last StreamChunk
			for chunk := range ch {
				last = chunk
			}
			if !errors.Is(last.Err, io.ErrUnexpectedEOF) || last.Done {
				t.Fatalf("truncated stream accepted: %+v", last)
			}
		})
	}
}
func TestPoeRefusalRetryAfterMissingKeyAndTimeout(t *testing.T) {
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"code":429,"message":"rate limit"}}`)
	})
	_, err := c.Generate(context.Background(), "bot", nil)
	var status *StatusError
	if !errors.As(err, &status) || status.RetryAfter != 2*time.Second {
		t.Fatalf("status=%v", err)
	}
	c.fallbackKey = ""
	_, err = c.Generate(context.Background(), "bot", nil)
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatal(err)
	}
	c = poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	})
	c.RequestTimeout = 10 * time.Millisecond
	_, err = c.Generate(context.Background(), "bot", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout=%v", err)
	}
}
func TestPoeNativeRejectsMalformedToolHistoryBeforeHTTP(t *testing.T) {
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("malformed tools reached HTTP") })
	c.ModelInfo = func(string) PoeModelInfo { return PoeModelInfo{NativeMessages: true} }
	_, err := c.Generate(context.Background(), "bot", []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "cmd", Arguments: "invalid"}}}})
	if err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestPoeChatAllowsToolErrorRecovery(t *testing.T) {
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body := readPoeBody(t, r)
		messages := body["messages"].([]any)
		calls := messages[0].(map[string]any)["tool_calls"].([]any)
		if calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != "broken JSON" {
			t.Error("original arguments were discarded")
		}
		if messages[1].(map[string]any)["tool_call_id"] != "c1" {
			t.Error("tool error lost its call ID")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Fixed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	})
	out, err := c.Generate(context.Background(), "bot", []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "run_script", Arguments: "broken JSON"}}}, {Role: "tool", ToolCallID: "c1", Content: "Invalid JSON; correct the arguments"}})
	if err != nil || out.Text != "Fixed" {
		t.Fatalf("tool recovery: %+v %v", out, err)
	}
}
