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
	"strings"
	"testing"
)

func TestPolzaClient_WebToolsRequestShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL

	drain := func(ctx context.Context) {
		ch, err := c.GenerateStream(ctx, "m", []Message{{Role: "user", Content: "hi"}})
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}

	drain(WithWebTools(context.Background(), WebTools{Tools: true}))
	tools, _ := body["tools"].([]any)
	if len(tools) != 3 || body["max_tool_calls"] != float64(WebMaxToolCalls) {
		t.Fatalf("tools = %v max_tool_calls = %v", body["tools"], body["max_tool_calls"])
	}
	search := tools[0].(map[string]any)
	if search["type"] != "polza:web_search" || search["parameters"].(map[string]any)["max_results"] != float64(10) {
		t.Errorf("search tool = %v, want polza:web_search with 10 results", search)
	}
	if fetch := tools[1].(map[string]any); fetch["type"] != "polza:web_fetch" {
		t.Errorf("fetch tool = %v", fetch)
	}
	if opts := body["stream_options"].(map[string]any); opts["include_server_tool_events"] != true {
		t.Errorf("stream_options = %v, want server tool events", opts)
	}
	if prefs, _ := body["provider"].(map[string]any); prefs["require_parameters"] != true {
		t.Errorf("provider = %v, want require_parameters so tools can't be dropped silently", body["provider"])
	}

	drain(WithWebTools(context.Background(), WebTools{SearchFirst: true}))
	if _, ok := body["web_search_options"].(map[string]any); !ok || body["tools"] != nil || body["provider"] != nil {
		t.Errorf("search-first body = %v, want web_search_options and no tools", body)
	}

	drain(context.Background())
	if body["tools"] != nil || body["web_search_options"] != nil || body["provider"] != nil || body["stream_options"].(map[string]any)["include_server_tool_events"] != nil {
		t.Errorf("plain request carried web settings: %v", body)
	}
}

// The event shape isn't documented, so the parser must cope with the
// plausible variants rather than one guess.
func TestParseServerToolEvents(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want []ToolEvent
	}{
		"flat call with object args": {
			`{"type":"tool_call","id":"c1","tool":"polza:web_search","arguments":{"query":"anthropic pricing"}}`,
			[]ToolEvent{{ID: "c1", Tool: "web_search", Phase: "call", Query: "anthropic pricing"}},
		},
		"nested openai-style call with string args": {
			`{"event":"tool_call","tool_call":{"id":"c2","type":"function","function":{"name":"polza:web_fetch","arguments":"{\"url\":\"https://anthropic.com/pricing\"}"}}}`,
			[]ToolEvent{{ID: "c2", Tool: "web_fetch", Phase: "call", URL: "https://anthropic.com/pricing"}},
		},
		"search result list": {
			`{"type":"tool_result","tool_call_id":"c1","name":"web_search","result":{"results":[{"title":"Pricing","url":"https://anthropic.com/pricing","snippet":"Opus costs"},{"title":"Docs","url":"https://docs.anthropic.com"}]}}`,
			[]ToolEvent{{ID: "c1", Tool: "web_search", Phase: "result", Results: []WebResult{{Title: "Pricing", URL: "https://anthropic.com/pricing", Snippet: "Opus costs"}, {Title: "Docs", URL: "https://docs.anthropic.com"}}}},
		},
		"result as JSON string, no tool name": {
			`{"type":"tool_result","tool_call_id":"c9","content":"[{\"title\":\"A\",\"link\":\"https://a.example\"}]"}`,
			[]ToolEvent{{ID: "c9", Phase: "result", Results: []WebResult{{Title: "A", URL: "https://a.example"}}}},
		},
		"event list and noise": {
			`{"events":[{"type":"step_end","step":1},{"type":"tool_call","tool":{"type":"polza:web_search"},"input":{"q":"go 1.25 release"}},{"type":"budget_exhausted"}]}`,
			[]ToolEvent{{Tool: "web_search", Phase: "call", Query: "go 1.25 release"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseServerToolEvents(json.RawMessage(tc.raw))
			if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
	if got := parseServerToolEvents(json.RawMessage(`not json`)); got != nil {
		t.Fatalf("garbage parsed into %+v", got)
	}
}

func TestPolzaClient_StreamCollectsToolActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"polza:web_search","arguments":"{\"query\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"polza docs\"}"}}]}}]}`,
			`{"choices":[],"polza":{"type":"tool_result","tool_call_id":"t1","result":[{"title":"Polza","url":"https://polza.ai"}]}}`,
			`{"choices":[{"delta":{"content":"Answer","annotations":[{"type":"url_citation","url_citation":{"url":"https://polza.ai","title":"Polza"}}]}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"server_tool_use":{"web_search_requests":1,"web_fetch_requests":0}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL

	ch, err := c.GenerateStream(WithWebTools(context.Background(), WebTools{Tools: true}), "m", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	var live []ToolEvent
	var final GenerateResult
	for chunk := range ch {
		if chunk.Tool != nil {
			live = append(live, *chunk.Tool)
		}
		if chunk.Done {
			final = chunk.Final
		}
	}
	if len(live) != 2 || live[0].Query != "polza docs" || live[0].Phase != "call" || live[1].Phase != "result" || len(live[1].Results) != 1 {
		t.Fatalf("live events = %+v", live)
	}
	if final.Text != "Answer" || len(final.ToolEvents) != 2 || len(final.Citations) != 1 || final.WebSearches != 1 {
		t.Fatalf("final = %+v", final)
	}
}

func TestPolzaClient_StreamBreaksParagraphAfterToolStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`{"choices":[{"delta":{"content":"Let me check."}}]}`,
			`{"choices":[],"polza":{"type":"tool_call","id":"s1","tool":"polza:web_search","arguments":{"query":"x"}}}`,
			`{"choices":[],"polza":{"type":"tool_result","id":"s1","tool":"polza:web_search","result":[]}}`,
			`{"choices":[{"delta":{"content":" It costs $4."}}]}`,
			`{"choices":[{"delta":{"content":" Done."}}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL

	ch, err := c.GenerateStream(WithWebTools(context.Background(), WebTools{Tools: true}), "m", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	var final GenerateResult
	for chunk := range ch {
		streamed.WriteString(chunk.Delta)
		if chunk.Done {
			final = chunk.Final
		}
	}
	const want = "Let me check.\n\nIt costs $4. Done."
	if streamed.String() != want || final.Text != want {
		t.Fatalf("streamed %q, final %q, want %q", streamed.String(), final.Text, want)
	}
}

func TestPolzaClient_LogsWhoServedAWebCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"provider\":\"amazon-bedrock\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"server_tool_use\":{\"web_search_requests\":2}}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL

	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	drain := func(ctx context.Context) {
		ch, err := c.GenerateStream(ctx, "anthropic/claude-x", []Message{{Role: "user", Content: "hi"}})
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}

	drain(WithWebTools(context.Background(), WebTools{Tools: true}))
	if got := logged.String(); !strings.Contains(got, "model=anthropic/claude-x provider=amazon-bedrock mode=tools") || !strings.Contains(got, "searches=2") {
		t.Fatalf("log = %q", got)
	}
	logged.Reset()
	drain(context.Background())
	if logged.Len() != 0 {
		t.Fatalf("a call without web access logged %q", logged.String())
	}
}
