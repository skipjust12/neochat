package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"neochat/pagestore"
	"neochat/provider"
	"neochat/router"
	"neochat/webfetch"
)

type batchSearcher func(context.Context, string, int) ([]provider.WebResult, error)

func (f batchSearcher) Search(ctx context.Context, q string, n int) ([]provider.WebResult, error) {
	return f(ctx, q, n)
}
func batchTool(id, name string, args any) provider.ToolCall {
	b, _ := json.Marshal(args)
	return provider.ToolCall{ID: id, Name: name, Arguments: string(b)}
}
func newBatchRun(t *testing.T) *webRun {
	t.Helper()
	s, _ := newTestServer(t, nil)
	return &webRun{s: s, req: chatRequest{UserID: "u1", Message: "important evidence", WebSearch: "auto"}, prepared: preparedRequest{conversationID: "c1"}, round: 1}
}

func TestWebBatchSearchParallelDedupAndSharedQueue(t *testing.T) {
	r := newBatchRun(t)
	started := make(chan string, 16)
	release := make(chan struct{})
	var active, peak, count atomic.Int32
	r.s.Searcher = batchSearcher(func(ctx context.Context, q string, _ int) ([]provider.WebResult, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		count.Add(1)
		started <- q
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if q == "bad" {
			return nil, errors.New("source failed")
		}
		return []provider.WebResult{{Title: q, URL: "https://example.com/" + q, Snippet: "evidence"}}, nil
	})
	calls := []provider.ToolCall{batchTool("a", "web_search", map[string]any{"queries": []string{"one", "two", "one"}}), batchTool("b", "web_search", map[string]any{"queries": []string{"two", "three", "bad"}})}
	done := make(chan []string, 1)
	go func() { done <- r.runTools(context.Background(), calls, nil) }()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("batch did not start independent searches concurrently")
		}
	}
	r2 := newBatchRun(t)
	r2.s = r.s
	done2 := make(chan []string, 1)
	go func() {
		done2 <- r2.runTools(context.Background(), []provider.ToolCall{batchTool("c", "web_search", map[string]any{"query": "five"})}, nil)
	}()
	select {
	case q := <-started:
		t.Fatal("server-wide concurrency exceeded: " + q)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	results := <-done
	result2 := <-done2
	if peak.Load() > 4 || count.Load() != 5 || r.searches != 4 || len(r.snapshot()) != 4 {
		t.Fatalf("peak=%d count=%d budget=%d activity=%d", peak.Load(), count.Load(), r.searches, len(r.snapshot()))
	}
	if len(result2) != 1 || !strings.Contains(results[1], "Error:") || !strings.Contains(results[1], "three") {
		t.Fatalf("partial failure lost other results: %v", results)
	}
	before := count.Load()
	r.runTools(context.Background(), []provider.ToolCall{batchTool("cached", "web_search", map[string]any{"query": "one"})}, nil)
	if count.Load() != before {
		t.Fatal("repeated query was not cached")
	}
}

func TestWebBatchFetchOffsetsCacheAndPartialFailure(t *testing.T) {
	var hits atomic.Int32
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if req.URL.Path == "/bad" {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("A", 20000)+"IMPORTANT TAIL")
	}))
	defer page.Close()
	r := newBatchRun(t)
	r.s.Fetcher = &webfetch.Fetcher{AllowPrivate: true}
	r.s.Pages = pagestore.NewInMemoryStore()
	args := map[string]any{"pages": []webPageRequest{{URL: page.URL + "/ok"}, {URL: page.URL + "/ok#same", Offset: 20000}, {URL: page.URL + "/bad"}}}
	results := r.runTools(context.Background(), []provider.ToolCall{batchTool("read", "web_fetch", args)}, nil)
	if hits.Load() != 2 || r.fetches != 3 {
		t.Fatalf("downloads=%d windows=%d", hits.Load(), r.fetches)
	}
	if !strings.Contains(results[0], "IMPORTANT TAIL") || !strings.Contains(results[0], "Error:") {
		t.Fatal("missing tail or independent error")
	}
	r.runTools(context.Background(), []provider.ToolCall{batchTool("again", "web_fetch", map[string]any{"url": page.URL + "/ok", "offset": 20000})}, nil)
	if hits.Load() != 2 || r.fetches != 3 {
		t.Fatal("repeat consumed budget or downloaded again")
	}
	cached, ok, err := r.s.Pages.Get(context.Background(), "u1", "c1", page.URL+"/ok", time.Now().Add(-time.Hour))
	if err != nil || !ok || !strings.HasSuffix(cached.Text, "IMPORTANT TAIL") {
		t.Fatal("full page was not preserved", err)
	}
}

func TestWebBudgetsAndBatchValidation(t *testing.T) {
	for _, mode := range []bool{false, true} {
		r := newBatchRun(t)
		r.req.SearchMode = mode
		r.s.Searcher = batchSearcher(func(context.Context, string, int) ([]provider.WebResult, error) { return nil, nil })
		searches, pages := r.webLimits()
		wantSearch, wantPage := 24, 12
		if mode {
			wantSearch, wantPage = 30, 20
		}
		if searches != wantSearch || pages != wantPage {
			t.Fatal(searches, pages)
		}
		for i := 0; i < searches; i++ {
			r.runTools(context.Background(), []provider.ToolCall{batchTool(fmt.Sprint(i), "web_search", map[string]any{"query": fmt.Sprint(i)})}, nil)
		}
		result, _ := r.planWebTool(batchTool("over", "web_search", map[string]any{"query": "over"}))
		if !strings.HasPrefix(result, "Search limit") {
			t.Fatal(result)
		}
		r.fetches = pages
		result, _ = r.planWebTool(batchTool("overpage", "web_fetch", map[string]any{"url": "https://example.com/new"}))
		if !strings.HasPrefix(result, "Page limit") {
			t.Fatal(result)
		}
		for _, args := range []any{map[string]any{"queries": []string{}}, map[string]any{"queries": []string{"a", "b", "c", "d", "e", "f"}}, map[string]any{"query": "a", "queries": []string{"b"}}} {
			if result, job := r.planWebTool(batchTool("invalid", "web_search", args)); job != nil || !strings.HasPrefix(result, "Error:") {
				t.Fatal("invalid batch accepted")
			}
		}
	}
}

func TestSearchModeCompactsAfterTwoFollowingSteps(t *testing.T) {
	r := newBatchRun(t)
	r.req.SearchMode = true
	r.webRecords = map[string]webRecord{"web": {round: 1, compact: "short source https://example.com"}}
	messages := []provider.Message{{Role: "assistant", ToolCalls: []provider.ToolCall{batchTool("web", "web_fetch", map[string]any{"url": "https://example.com", "notes": "old cumulative notes"})}}, {Role: "tool", ToolCallID: "web", Content: strings.Repeat("full evidence ", 10000)}, {Role: "tool", ToolCallID: "script", Content: "script output must stay"}}
	for _, round := range []int{2, 3} {
		if r.compactWebMessages(messages, round)[1].Content != messages[1].Content {
			t.Fatal("compacted before the model could read twice")
		}
	}
	compact := r.compactWebMessages(messages, 4)
	if compact[1].Content != "short source https://example.com" || strings.Contains(compact[0].ToolCalls[0].Arguments, "old cumulative") || compact[2].Content != messages[2].Content {
		t.Fatal("compaction lost sources or touched other tools")
	}
	if len(messages[1].Content) < 1000 {
		t.Fatal("mutated source messages")
	}
	r.req.SearchMode = false
	if len(r.compactWebMessages(messages, 4)[1].Content) < 1000 {
		t.Fatal("normal mode compacted as early as Search mode")
	}
	if len(r.compactWebMessages(messages, 7)[1].Content) > 1000 {
		t.Fatal("normal mode never compacted")
	}
	text := strings.Repeat("unrelated background\n", 100) + "important evidence: total 12345; source https://example.com\n" + strings.Repeat("filler\n", 100)
	if excerpt := relevantExcerpt(text, r.req.Message, 1800); !strings.Contains(excerpt, "12345") {
		t.Fatal("relevant evidence was omitted")
	}
}

func TestSearchModeCanUseThirtySearchesAndTwentyPages(t *testing.T) {
	r := newBatchRun(t)
	r.req.SearchMode = true
	r.req.WebSearch = "off"
	r.model = router.Model{ID: "test", ToolCalling: true}
	r.s.Searcher = batchSearcher(func(context.Context, string, int) ([]provider.WebResult, error) {
		return []provider.WebResult{{URL: "https://example.com", Snippet: strings.Repeat("evidence ", 30)}}, nil
	})
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("important evidence ", 1500))
	}))
	defer page.Close()
	r.s.Fetcher = &webfetch.Fetcher{AllowPrivate: true}
	calls := 0
	seenMode, seenCompact, seenNotes := false, false, false
	generate := func(ctx context.Context, messages []provider.Message) (provider.GenerateResult, error) {
		calls++
		for _, m := range messages {
			seenMode = seenMode || m.Content == searchModeNote
			seenCompact = seenCompact || strings.Contains(m.Content, "Older search output compacted")
			seenNotes = seenNotes || strings.Contains(m.Content, "Working research notes")
		}
		if calls <= 30 {
			return provider.GenerateResult{ToolCalls: []provider.ToolCall{batchTool(fmt.Sprint(calls), "web_search", map[string]any{"query": fmt.Sprint(calls), "notes": "verified evidence https://example.com"})}}, nil
		}
		if calls <= 50 {
			return provider.GenerateResult{ToolCalls: []provider.ToolCall{batchTool(fmt.Sprint(calls), "web_fetch", map[string]any{"url": fmt.Sprintf("%s/%d", page.URL, calls)})}}, nil
		}
		return provider.GenerateResult{Text: "final evidence"}, nil
	}
	answer, err := r.s.answer(context.Background(), r.req, r.prepared, r.model, nil, generate, nil)
	if err != nil || calls != 51 || !seenMode || !seenCompact || !seenNotes || len(answer.Activity) != 50 {
		t.Fatalf("calls=%d mode=%t compact=%t notes=%t steps=%d err=%v", calls, seenMode, seenCompact, seenNotes, len(answer.Activity), err)
	}
}

func TestSearchModePersistsAndRegenerates(t *testing.T) {
	vendor := &scriptedPolza{reply: func(map[string]any) (int, []string) {
		return 200, []string{textChunk("Researched answer."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := inlineImageRequest()
	req.SearchMode = true
	req.WebSearch = "off"
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	history, err := s.Conversations.History(context.Background(), "u1", done.ConversationID, 0)
	if err != nil || len(history) != 2 || !history[0].SearchMode {
		t.Fatal("Search mode was not stored", err)
	}
	headers := map[string]string{providerKeyHeader: "test-key"}
	regenerated := doneEvent(t, sendStream(t, s.Mux(), "/chat/regenerate/stream", strings.TrimPrefix(authHeader(t, s, "u1", "pro"), "Bearer "), headers, map[string]any{"conversation_id": done.ConversationID, "message_id": history[1].ID, "requested_mode": "manual", "manual_model_id": "gpt-6-luna", "web_search": "off"}))
	if regenerated.ResponseText != "Researched answer." {
		t.Fatal(regenerated.ResponseText)
	}
	calls := vendor.mainCalls()
	if len(calls) != 2 || !hasSystemMessage(calls[1], searchModeNote) || calls[1]["tools"] == nil {
		t.Fatal("regeneration lost Search mode")
	}
}

func TestWebQueueCancellationDuringCooldown(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	_ = s.withWebSlot(ctx, func() error { return fmt.Errorf("HTTP 429") })
	s.webGate.mu.Lock()
	cool := time.Now().Before(s.webGate.until)
	s.webGate.mu.Unlock()
	if !cool {
		t.Fatal("rate limit did not enable cooldown")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.withWebSlot(canceled, func() error { t.Error("canceled web work ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled queue did not stop", err)
	}
}
