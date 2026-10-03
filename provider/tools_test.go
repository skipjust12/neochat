package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// recordingServer answers every chat call with the given SSE lines and
// keeps the last request body.
func recordingServer(t *testing.T, lines ...string) (*PolzaClient, *map[string]any) {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range lines {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL
	return c, &body
}

func streamPolza(t *testing.T, c *PolzaClient, ctx context.Context, messages []Message) (string, GenerateResult) {
	t.Helper()
	ch, err := c.GenerateStream(ctx, "m", messages)
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	var final GenerateResult
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		streamed.WriteString(chunk.Delta)
		if chunk.Done {
			final = chunk.Final
		}
	}
	return streamed.String(), final
}

func TestPolzaClient_ToolRequestShape(t *testing.T) {
	c, body := recordingServer(t, `{"choices":[{"delta":{"content":"ok"}}]}`)
	tools := []FunctionTool{{Name: "web_fetch", Description: "Read a page", Parameters: map[string]any{"type": "object"}}}
	ctx := WithFunctionTools(context.Background(), tools)
	ctx = WithWebPlugin(ctx, WebPlugin{Engine: "yandex", MaxResults: 10, SearchPrompt: "polza docs"})
	ctx = WithReasoning(ctx, Reasoning{Disabled: true, Adaptive: true})
	messages := []Message{
		{Role: "user", Content: "read polza.ai"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "web_fetch", Arguments: `{"url":"https://polza.ai"}`}}, Reasoning: "need the page", ReasoningDetails: json.RawMessage(`[{"type":"reasoning.text","text":"need the page","signature":"sig"}]`)},
		{Role: "tool", ToolCallID: "c1", Content: "Polza home page"},
	}
	streamPolza(t, c, ctx, messages)

	tool := (*body)["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["function"].(map[string]any)["name"] != "web_fetch" {
		t.Errorf("tool = %v", tool)
	}
	plugin := (*body)["plugins"].([]any)[0].(map[string]any)
	if plugin["id"] != "web" || plugin["engine"] != "yandex" || plugin["max_results"] != float64(10) || plugin["search_prompt"] != "polza docs" {
		t.Errorf("plugin = %v", plugin)
	}
	if r := (*body)["reasoning"].(map[string]any); r["type"] != "disabled" {
		t.Errorf("reasoning = %v, want disabled", r)
	}
	wire := (*body)["messages"].([]any)
	assistant, toolTurn := wire[1].(map[string]any), wire[2].(map[string]any)
	if content, present := assistant["content"]; !present || content != nil {
		t.Errorf("assistant content = %v (present %v), want null for a tool-only turn", content, present)
	}
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "c1" || call["type"] != "function" || call["function"].(map[string]any)["arguments"] != `{"url":"https://polza.ai"}` {
		t.Errorf("tool call = %v", call)
	}
	if assistant["reasoning"] != "need the page" || assistant["reasoning_details"].([]any)[0].(map[string]any)["signature"] != "sig" {
		t.Errorf("assistant reasoning not sent back: %v", assistant)
	}
	if toolTurn["role"] != "tool" || toolTurn["tool_call_id"] != "c1" || toolTurn["content"] != "Polza home page" {
		t.Errorf("tool turn = %v", toolTurn)
	}

	streamPolza(t, c, context.Background(), []Message{{Role: "user", Content: "hi"}})
	if (*body)["tools"] != nil || (*body)["plugins"] != nil || (*body)["reasoning"] != nil {
		t.Errorf("plain request carried tools, plugins or reasoning: %v", *body)
	}
	if user := (*body)["messages"].([]any)[0].(map[string]any); user["tool_calls"] != nil || user["tool_call_id"] != nil {
		t.Errorf("plain message carried tool fields: %v", user)
	}

	streamPolza(t, c, WithReasoning(context.Background(), Reasoning{Disabled: true}), []Message{{Role: "user", Content: "hi"}})
	if r := (*body)["reasoning"].(map[string]any); r["effort"] != "none" {
		t.Errorf("disabled effort reasoning = %v, want effort none", r)
	}
}

func TestPolzaClient_StreamCollectsToolCalls(t *testing.T) {
	c, _ := recordingServer(t,
		`{"choices":[{"delta":{"reasoning":"Let me ","reasoning_details":[{"type":"reasoning.text","index":0,"text":"Let me "}]}}]}`,
		`{"choices":[{"delta":{"reasoning":"look.","reasoning_details":[{"type":"reasoning.text","index":0,"text":"look.","signature":"s1"}]}}]}`,
		`{"choices":[{"delta":{"content":"Checking."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"web_search","arguments":"{\"query\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"web_fetch","arguments":"{\"url\":\"https://x.dev\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go 1.25\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"provider":"openai","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
	)
	streamed, final := streamPolza(t, c, WithFunctionTools(context.Background(), []FunctionTool{{Name: "web_search"}}), []Message{{Role: "user", Content: "hi"}})

	if streamed != "Checking." || final.Text != "Checking." {
		t.Fatalf("text = %q / %q, want only the answer text (reasoning is never shown)", streamed, final.Text)
	}
	want := []ToolCall{{ID: "a", Name: "web_search", Arguments: `{"query":"go 1.25"}`}, {ID: "b", Name: "web_fetch", Arguments: `{"url":"https://x.dev"}`}}
	if !reflect.DeepEqual(final.ToolCalls, want) || final.FinishReason != "tool_calls" {
		t.Fatalf("tool calls = %+v finish = %q", final.ToolCalls, final.FinishReason)
	}
	if final.Reasoning != "Let me look." {
		t.Errorf("reasoning = %q", final.Reasoning)
	}
	var details []map[string]any
	if err := json.Unmarshal(final.ReasoningDetails, &details); err != nil || len(details) != 1 || details[0]["text"] != "Let me look." || details[0]["signature"] != "s1" {
		t.Errorf("reasoning details = %s, want the pieces merged into one entry with its signature", final.ReasoningDetails)
	}
	if final.InputTokens != 10 || final.OutputTokens != 4 {
		t.Errorf("usage = %d/%d", final.InputTokens, final.OutputTokens)
	}
}

func TestPolzaClient_ParagraphBreakContinuesAnAnswer(t *testing.T) {
	c, _ := recordingServer(t,
		`{"choices":[{"delta":{"content":" \n"}}]}`,
		`{"choices":[{"delta":{"content":" It costs $4."}}]}`,
		`{"choices":[{"delta":{"content":" Done."}}]}`,
	)
	streamed, final := streamPolza(t, c, WithParagraphBreak(context.Background()), []Message{{Role: "user", Content: "hi"}})
	const want = "\n\nIt costs $4. Done."
	if streamed != want || final.Text != want {
		t.Fatalf("streamed %q, final %q, want %q", streamed, final.Text, want)
	}
	streamed, _ = streamPolza(t, c, context.Background(), []Message{{Role: "user", Content: "hi"}})
	if strings.HasPrefix(streamed, "\n\n") {
		t.Fatalf("a fresh answer got a paragraph break: %q", streamed)
	}
}

func TestPolzaClient_WebPluginCitations(t *testing.T) {
	c, _ := recordingServer(t,
		`{"provider":"yandex","choices":[{"delta":{"content":"Answer","annotations":[{"type":"url_citation","url_citation":{"url":"https://polza.ai","title":"Polza","content":"Aggregator"}}]}}]}`,
		`{"choices":[{"delta":{"annotations":[{"type":"url_citation","url_citation":{"url":"https://polza.ai","title":"Polza"}},{"type":"url_citation","url_citation":{"url":"https://polza.ai/docs","title":"Docs"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"server_tool_use":{"web_search_requests":1}}}`,
	)
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	_, final := streamPolza(t, c, WithWebPlugin(context.Background(), WebPlugin{Engine: "yandex"}), []Message{{Role: "user", Content: "hi"}})
	if len(final.Citations) != 2 || final.Citations[0].Snippet != "Aggregator" || final.WebSearches != 1 {
		t.Fatalf("final = %+v", final)
	}
	if got := logged.String(); !strings.Contains(got, "web plugin model=m provider=yandex engine=yandex results=2") {
		t.Fatalf("log = %q", got)
	}
	logged.Reset()
	streamPolza(t, c, context.Background(), []Message{{Role: "user", Content: "hi"}})
	if logged.Len() != 0 {
		t.Fatalf("a call without the plugin logged %q", logged.String())
	}
}

func TestPolzaClient_GenerateReturnsToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":null,"reasoning":"hmm","tool_calls":[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls","reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	defer srv.Close()
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL
	result, err := c.Generate(context.Background(), "m", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "web_search" || result.FinishReason != "tool_calls" || result.Reasoning != "hmm" || !strings.Contains(string(result.ReasoningDetails), "opaque") {
		t.Fatalf("result = %+v", result)
	}
}

func TestReasoningDetailsMerge(t *testing.T) {
	var d reasoningDetails
	d.add([]json.RawMessage{json.RawMessage(`{"type":"reasoning.summary","summary":"a"}`)})
	d.add([]json.RawMessage{json.RawMessage(`{"type":"reasoning.text","index":1,"text":"x"}`), json.RawMessage(`not json`)})
	d.add([]json.RawMessage{json.RawMessage(`{"type":"reasoning.text","index":1,"text":"y","signature":"s"}`)})
	var got []map[string]any
	if err := json.Unmarshal(d.raw(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["summary"] != "a" || got[1]["text"] != "xy" || got[1]["signature"] != "s" {
		t.Fatalf("merged = %v", got)
	}
	var empty reasoningDetails
	if empty.raw() != nil {
		t.Fatal("no details should send nothing back")
	}
}

func TestPolzaClient_GenerateParagraphBreak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"  The answer."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	defer srv.Close()
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL
	result, err := c.Generate(WithParagraphBreak(context.Background()), "m", []Message{{Role: "user", Content: "hi"}})
	if err != nil || result.Text != "\n\nThe answer." {
		t.Fatalf("text = %q, %v", result.Text, err)
	}
}
