package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// marks lists, per message, where cache_control sits: -1 for nowhere, else
// the content part's index.
func marks(t *testing.T, body []byte) []int {
	t.Helper()
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	out := make([]int, len(req.Messages))
	for i, m := range req.Messages {
		out[i] = -1
		var parts []map[string]any
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for j, p := range parts {
			if p["cache_control"] != nil {
				if out[i] != -1 {
					t.Errorf("message %d has more than one cache mark", i)
				}
				out[i] = j
			}
		}
	}
	return out
}

func TestBuildRequest_MarksCachePointsForClaude(t *testing.T) {
	c := NewPolzaClient("k")
	messages := []Message{
		{Role: "system", Content: "persona"},
		{Role: "system", Content: "custom instructions"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "answer two"},
		{Role: "user", Parts: []Part{
			{Type: PartText, Text: "third, with a picture and a document"},
			{Type: PartImage, MIME: "image/webp", Data: []byte("img")},
			{Type: PartFile, MIME: "application/pdf", Name: "a.pdf", Data: []byte("%PDF")},
		}},
	}

	body, _ := json.Marshal(c.buildRequest(context.Background(), "anthropic/claude-sonnet-5.5", messages, true))
	got := marks(t, body)
	// The last leading system message, the user turn before the last, and
	// the last one's image (the document after it is left unmarked).
	want := []int{-1, 0, -1, -1, 0, -1, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cache marks = %v, want %v\nbody: %s", got, want, body)
		}
	}
	var wire struct {
		Messages []struct {
			Content []struct {
				CacheControl map[string]string `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &wire)
	if cc := wire.Messages[1].Content[0].CacheControl; cc["type"] != "ephemeral" {
		t.Errorf("cache_control = %v, want type ephemeral", cc)
	}

	body, _ = json.Marshal(c.buildRequest(context.Background(), "openai/gpt-6-luna", messages, true))
	for i, mark := range marks(t, body) {
		if mark != -1 {
			t.Errorf("message %d marked for a model that caches on its own", i)
		}
	}
	var plain struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &plain)
	if _, ok := plain.Messages[0].Content.(string); !ok {
		t.Errorf("unmarked system content = %T, want a plain string", plain.Messages[0].Content)
	}
}

func TestBuildRequest_CacheMarksSkipEmptyAndToolTurns(t *testing.T) {
	c := NewPolzaClient("k")
	messages := []Message{
		{Role: "user", Content: "look this up"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "web_search", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "results"},
		{Role: "user", Content: "   "},
	}
	body, _ := json.Marshal(c.buildRequest(context.Background(), "anthropic/claude-haiku-4.5", messages, false))
	got := marks(t, body)
	want := []int{0, -1, -1, -1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cache marks = %v, want %v\nbody: %s", got, want, body)
		}
	}
}

func TestPolzaClient_ReadsCacheUsageAndCost(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		read  int
		write int
		cost  float64
	}{
		{"numbers", `{"prompt_tokens": 1500, "completion_tokens": 100, "prompt_tokens_details": {"cached_tokens": 1400, "cache_write_tokens": 0}, "cost_rub": 2.5, "cost": 2.5}`, 1400, 0, 2.5},
		{"cost alias only", `{"prompt_tokens": 1500, "completion_tokens": 100, "prompt_tokens_details": {"cached_tokens": 0, "cache_write_tokens": 1450}, "cost": 3.1}`, 0, 1450, 3.1},
		{"strings and floats", `{"prompt_tokens": 1500, "completion_tokens": 100, "prompt_tokens_details": {"cached_tokens": "1200", "cache_write_tokens": 10.0}, "cost_rub": "0.75"}`, 1200, 10, 0.75},
		{"odd values", `{"prompt_tokens": 1500, "completion_tokens": 100, "prompt_tokens_details": {"cached_tokens": null, "cache_write_tokens": {}}, "cost_rub": {"total": 1}}`, 0, 0, 0},
		{"absent", `{"prompt_tokens": 1500, "completion_tokens": 100}`, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices": [{"message": {"content": "ok"}}], "usage": ` + tc.usage + `}`))
			}))
			defer srv.Close()
			c := NewPolzaClient("k")
			c.BaseURL = srv.URL
			result, err := c.Generate(context.Background(), "anthropic/claude-haiku-4.5", []Message{{Role: "user", Content: "hi"}})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if result.InputTokens != 1500 || result.OutputTokens != 100 {
				t.Errorf("tokens = %d/%d, want 1500/100", result.InputTokens, result.OutputTokens)
			}
			if result.CachedTokens != tc.read || result.CacheWriteTokens != tc.write || result.CostRUB != tc.cost {
				t.Errorf("cache read/write/cost = %d/%d/%v, want %d/%d/%v", result.CachedTokens, result.CacheWriteTokens, result.CostRUB, tc.read, tc.write, tc.cost)
			}
		})
	}
}

func TestPolzaClient_StreamReadsCacheUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5000,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":4800,\"cache_write_tokens\":150},\"cost_rub\":0.42,\"server_tool_use\":{\"web_search_requests\":2}}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL
	ch, err := c.GenerateStream(context.Background(), "anthropic/claude-haiku-4.5", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var final GenerateResult
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		if chunk.Done {
			final = chunk.Final
		}
	}
	if final.Text != "hi" || final.InputTokens != 5000 || final.OutputTokens != 7 {
		t.Errorf("final = %q %d/%d, want hi 5000/7", final.Text, final.InputTokens, final.OutputTokens)
	}
	if final.CachedTokens != 4800 || final.CacheWriteTokens != 150 || final.CostRUB != 0.42 || final.WebSearches != 2 {
		t.Errorf("cache read/write/cost/searches = %d/%d/%v/%d, want 4800/150/0.42/2", final.CachedTokens, final.CacheWriteTokens, final.CostRUB, final.WebSearches)
	}
}
