package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"neochat/costlog"
	"neochat/provider"
)

func TestPoeSearchMergesSingletonsAndKeepsSearching(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/Perplexity-Search" || req.Header.Get("Authorization") != "Bearer user-poe-key" {
			t.Errorf("route/auth %s", req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		query := body["query"].([]any)
		if len(query) != 1 || body["tools"] != nil {
			t.Error("search received chat history/tools")
		}
		turn := query[0].(map[string]any)
		params := turn["parameters"].(map[string]any)
		q := params["query"].(string)
		multi, _ := params["multi_query"].(string)
		n := calls.Add(1)
		if n <= 2 && len(strings.Split(multi, "\n")) != map[bool]int{true: 5, false: 2}[strings.HasPrefix(q, "q0")] {
			t.Errorf("unexpected native batch %q", multi)
		}
		if params["max_results"] != float64(10) {
			t.Error(params)
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: json\ndata: {\"results\":[{\"title\":\"OpenAI official\",\"url\":\"https://openai.com/\",\"snippet\":\"Verified search evidence\"}]}\n\nevent: done\ndata: {}\n\n")
	}))
	defer upstream.Close()
	r := newBatchRun(t)
	r.req.providerKey = "user-poe-key"
	r.prepared.plan = r.s.Plans["pro"]
	client := provider.NewPoeClient("")
	client.BotQuery = true
	client.BotBaseURL = upstream.URL
	r.s.PoeSearch = client
	var operations []provider.ToolCall
	for i := 0; i < 7; i++ {
		operations = append(operations, batchTool(fmt.Sprint(i), "web_search", map[string]any{"query": fmt.Sprintf("q%d", i)}))
	}
	operations = append(operations, batchTool("duplicate", "web_search", map[string]any{"queries": []string{"q0", "q1"}}))
	finished := make(chan []string, 1)
	go func() { finished <- r.runTools(context.Background(), operations, nil) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("native batches did not start")
		}
	}
	pending := r.snapshot()
	if len(pending) != 7 {
		t.Fatal("lost query records", len(pending))
	}
	// snapshot marks unfinished work as stopped for persistence; live activity
	// must remain pending until the real provider response, never Thinking.
	r.mu.Lock()
	for _, step := range r.activity {
		if !step.Pending || step.Tool != "web_search" {
			t.Error("search state ended before upstream reply")
		}
	}
	r.mu.Unlock()
	close(release)
	results := <-finished
	if calls.Load() != 2 || r.searches != 7 {
		t.Fatalf("calls=%d queries=%d", calls.Load(), r.searches)
	}
	for _, step := range r.snapshot() {
		if step.Error != "" || len(step.Results) != 1 || step.Output == "" {
			t.Fatalf("lost output/sources: %+v", step)
		}
	}
	if !strings.Contains(results[0], "Verified search evidence") {
		t.Fatal("model lost raw evidence")
	}
	r.runTools(context.Background(), []provider.ToolCall{batchTool("later", "web_search", map[string]any{"queries": []string{"follow-up"}})}, nil)
	if calls.Load() != 3 {
		t.Fatal("later searches were blocked")
	}
	entries := r.s.CostLog.(*costlog.InMemoryStore).Entries()
	if len(entries) != 3 {
		t.Fatalf("accounting must be per provider request: %+v", entries)
	}
	r.runTools(context.Background(), []provider.ToolCall{batchTool("cached", "web_search", map[string]any{"query": "q0"})}, nil)
	if calls.Load() != 3 {
		t.Fatal("cache reissued paid search")
	}
}

func TestPoeSearchEvidenceFormats(t *testing.T) {
	for _, raw := range []string{`{"results":[{"url":"https://openai.com/","title":"OpenAI"}]}`, "```json\n{\"queries\":[{\"results\":[{\"url\":\"https://openai.com/\",\"title\":\"OpenAI\"}]}]}\n```", "[OpenAI](https://openai.com/)", "Source: https://openai.com/"} {
		links := parsePoeSearchLinks(raw, nil)
		if len(links) != 1 || links[0].URL != "https://openai.com/" {
			t.Fatalf("parse failed: %s %+v", raw, links)
		}
	}
	if len(parsePoeSearchLinks(`{"results":[{"url":"file:///secret"}]}`, nil)) != 0 {
		t.Fatal("unsafe source URL accepted")
	}
}

func TestPoeSearchRefusalIsNotRetriedOrReplaced(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":"denied"}`)
	}))
	defer upstream.Close()
	r := newBatchRun(t)
	r.req.providerKey = "key"
	r.prepared.plan = r.s.Plans["pro"]
	client := provider.NewPoeClient("")
	client.BotQuery = true
	client.BotBaseURL = upstream.URL
	r.s.PoeSearch = client
	result := r.runTools(context.Background(), []provider.ToolCall{batchTool("s", "web_search", map[string]any{"queries": []string{"one", "two"}})}, nil)
	if calls.Load() != 1 || !strings.Contains(result[0], "Error:") {
		t.Fatal("unexpected retry or silent fallback", calls.Load(), result)
	}
	if len(r.s.CostLog.(*costlog.InMemoryStore).Entries()) != 0 {
		t.Fatal("known refusal was charged")
	}
}
