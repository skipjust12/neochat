package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"neochat/conversation"
	"neochat/pagestore"
	"neochat/provider"
	"neochat/webfetch"
)

// scriptedPolza is a Polza stand-in for the web tool loop: reply decides
// each chat call's answer from its request body. Streamed calls get the
// returned lines as SSE; plain calls (the web search helper) get the
// first line as the JSON body.
type scriptedPolza struct {
	mu     sync.Mutex
	bodies []map[string]any
	reply  func(body map[string]any) (status int, lines []string)
}

func (p *scriptedPolza) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	p.mu.Lock()
	p.bodies = append(p.bodies, body)
	p.mu.Unlock()
	status, lines := p.reply(body)
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		fmt.Fprint(w, lines[0])
		return
	}
	if body["stream"] != true {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, lines[0])
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, line := range lines {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (p *scriptedPolza) calls() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.bodies...)
}

// mainCalls are the answer's own model calls (not search helpers).
func (p *scriptedPolza) mainCalls() []map[string]any {
	var out []map[string]any
	for _, b := range p.calls() {
		if b["plugins"] == nil || b["stream"] == true {
			out = append(out, b)
		}
	}
	return out
}

func newWebServer(t *testing.T, vendor *scriptedPolza) *Server {
	t.Helper()
	s := newDirectServer(t, &polzaStandIn{})
	srv := httptest.NewServer(vendor)
	t.Cleanup(srv.Close)
	client := provider.NewPolzaClient("")
	client.BaseURL = srv.URL
	s.Generators = map[string]provider.Client{"openai": client, "anthropic": client, "deepseek": client}
	s.Fetcher = &webfetch.Fetcher{AllowPrivate: true}
	s.Pages = pagestore.NewInMemoryStore()
	return s
}

func textChunk(text string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
	return string(b)
}

func toolCallChunk(calls ...[3]string) string {
	var list []any
	for i, c := range calls {
		list = append(list, map[string]any{"index": i, "id": c[0], "function": map[string]any{"name": c[1], "arguments": c[2]}})
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": list}, "finish_reason": "tool_calls"}}})
	return string(b)
}

const usageChunk = `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20}}`

// searchHelperReply answers a web search helper call: ten results as
// url_citation annotations, plus the model's JSON copy of them.
func searchHelperReply(n int) string {
	var annotations, copied []any
	for i := 1; i <= n; i++ {
		url := fmt.Sprintf("https://news.example/item-%d", i)
		annotations = append(annotations, map[string]any{"type": "url_citation", "url_citation": map[string]any{"url": url, "title": fmt.Sprintf("Result %d", i)}})
		copied = append(copied, map[string]any{"title": fmt.Sprintf("Result %d", i), "url": url, "snippet": fmt.Sprintf("Snippet %d", i)})
	}
	text, _ := json.Marshal(map[string]any{"results": copied})
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": string(text), "annotations": annotations}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 900, "completion_tokens": 300},
	})
	return string(b)
}

func toolMessages(body map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range body["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "tool" {
			out = append(out, msg)
		}
	}
	return out
}

func toolNames(body map[string]any) []string {
	var names []string
	tools, _ := body["tools"].([]any)
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return names
}

func hasSystemMessage(body map[string]any, content string) bool {
	for _, m := range body["messages"].([]any) {
		msg := m.(map[string]any)
		if msg["role"] == "system" && msg["content"] == content {
			return true
		}
	}
	return false
}

func hasSystemPrefix(body map[string]any, prefix string) bool {
	for _, m := range body["messages"].([]any) {
		msg := m.(map[string]any)
		if text, _ := msg["content"].(string); msg["role"] == "system" && strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func lastActivity(events map[string][]any) []conversation.ToolActivity {
	list := events["activity"]
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1].(map[string]any)["activity"].([]conversation.ToolActivity)
}

const articleHTML = `<html><head><title>GPT-6 is here</title></head><body><nav>menu</nav><main><h1>GPT-6</h1><p>OpenAI released GPT-6 Sol and GPT-6 Luna today. ` + `They are reasoning models with a one million token context window and new tool use.</p><p>Pricing starts at four dollars per million input tokens for Sol and much less for Luna.</p></main></body></html>`

func TestWebLoopSearchesReadsAndAnswers(t *testing.T) {
	var pageHits atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, articleHTML)
	}))
	defer site.Close()
	pageURL := site.URL + "/blog/gpt-6"

	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if body["plugins"] != nil {
			return 200, []string{searchHelperReply(10)}
		}
		if len(toolMessages(body)) == 0 {
			return 200, []string{
				textChunk("Let me look that up."),
				toolCallChunk([3]string{"s1", "web_search", `{"query":"latest OpenAI model"}`}, [3]string{"f1", "web_fetch", `{"url":"` + pageURL + `#top"}`}),
				usageChunk,
			}
		}
		return 200, []string{textChunk(" The latest is GPT-6 Sol."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "какая модель опенаи последняя", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "auto"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	done := events["done"][0].(chatResponse)
	if done.ResponseText != "Let me look that up.\n\nThe latest is GPT-6 Sol." {
		t.Fatalf("response = %q", done.ResponseText)
	}
	if len(done.Activity) != 2 {
		t.Fatalf("activity = %+v", done.Activity)
	}
	search, fetch := done.Activity[0], done.Activity[1]
	if search.Tool != "web_search" || search.Query != "latest OpenAI model" || len(search.Results) != 10 || search.Results[3].Snippet != "Snippet 4" || search.Pending {
		t.Errorf("search step = %+v", search)
	}
	if fetch.Tool != "web_fetch" || fetch.URL != pageURL || fetch.Title != "GPT-6 is here" || !strings.HasPrefix(fetch.Excerpt, "GPT-6 OpenAI released") || fetch.Chars < 200 || fetch.Pending {
		t.Errorf("fetch step = %+v", fetch)
	}
	if done.ThoughtMS <= 0 {
		t.Errorf("thought_ms = %d", done.ThoughtMS)
	}

	// Live: each step shows up running, then done.
	var sawPendingSearch, sawPendingFetch bool
	for _, e := range events["activity"] {
		for _, step := range e.(map[string]any)["activity"].([]conversation.ToolActivity) {
			sawPendingSearch = sawPendingSearch || (step.Tool == "web_search" && step.Pending)
			sawPendingFetch = sawPendingFetch || (step.Tool == "web_fetch" && step.Pending)
		}
	}
	if !sawPendingSearch || !sawPendingFetch {
		t.Errorf("steps never streamed as running: search %v fetch %v", sawPendingSearch, sawPendingFetch)
	}

	// What the vendor saw.
	main := vendor.mainCalls()
	if len(main) != 2 {
		t.Fatalf("main calls = %d, want the tool call and the answer", len(main))
	}
	if tools, _ := main[0]["tools"].([]any); len(tools) != 3 || !hasSystemPrefix(main[0], "You can search the web") {
		t.Errorf("first call: tools %v, web note %v", main[0]["tools"], hasSystemPrefix(main[0], "You can search the web"))
	}
	results := toolMessages(main[1])
	if len(results) != 2 || results[0]["tool_call_id"] != "s1" || results[1]["tool_call_id"] != "f1" {
		t.Fatalf("tool results = %v", results)
	}
	if text := results[0]["content"].(string); !strings.HasPrefix(text, "1. Result 1\nhttps://news.example/item-1\nSnippet 1") {
		t.Errorf("search result text = %q", text)
	}
	if text := results[1]["content"].(string); !strings.Contains(text, "Title: GPT-6 is here") || !strings.Contains(text, "Pricing starts at four dollars") || strings.Contains(text, "menu") {
		t.Errorf("page text = %q", text)
	}
	var helper map[string]any
	for _, b := range vendor.calls() {
		if b["plugins"] != nil {
			helper = b
		}
	}
	plugin := helper["plugins"].([]any)[0].(map[string]any)
	if plugin["id"] != "web" || plugin["engine"] != "yandex" || plugin["max_results"] != float64(10) || plugin["search_prompt"] != "latest OpenAI model" || helper["model"] != "openai/gpt-6-luna" || helper["reasoning"].(map[string]any)["effort"] != "none" {
		t.Errorf("search helper call = %v", helper)
	}

	// Stored: the version with its steps and timing, and the page itself.
	history, _ := s.Conversations.History(context.Background(), "u1", done.ConversationID, 0)
	version := history[len(history)-1].Versions[0]
	if len(version.Activity) != 2 || version.ThoughtMS <= 0 {
		t.Errorf("stored version = %+v", version)
	}
	if page, ok, _ := s.Pages.Get(context.Background(), "u1", done.ConversationID, pageURL, time.Time{}); !ok || page.Title != "GPT-6 is here" {
		t.Errorf("page not stored: %+v %v", page, ok)
	}
	if pageHits.Load() != 1 {
		t.Errorf("page fetched %d times", pageHits.Load())
	}
}

func TestWebFetchReadsLongPagesInPartsFromTheStore(t *testing.T) {
	var hits atomic.Int32
	long := strings.Repeat("слово ", 9000) // 53999 characters once the trailing space is trimmed
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, long)
	}))
	defer site.Close()

	step := 0
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		n := len(toolMessages(body))
		switch {
		case n == 0:
			return 200, []string{toolCallChunk([3]string{"a", "web_fetch", `{"url":"` + site.URL + `/doc"}`}), usageChunk}
		case n == 1:
			return 200, []string{toolCallChunk([3]string{"b", "web_fetch", `{"url":"` + site.URL + `/doc","offset":20000}`}), usageChunk}
		}
		step++
		return 200, []string{textChunk("done"), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "read it", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "auto"}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("site hit %d times, want once (the second part comes from the store)", hits.Load())
	}
	last := vendor.mainCalls()[2]
	parts := toolMessages(last)
	if first := parts[0]["content"].(string); !strings.Contains(first, "[Characters 0-20000 of 53999. Call web_fetch with offset=20000 to read on.]") {
		t.Errorf("first part header: %q", first[:200])
	}
	if second := parts[1]["content"].(string); !strings.Contains(second, "[Characters 20000-40000 of 53999.") {
		t.Errorf("second part header: %q", second[:200])
	}

	// Incognito chats keep nothing.
	s2 := newWebServer(t, &scriptedPolza{reply: vendor.reply})
	step = 0
	req.Incognito = true
	if _, err := collectStream(s2, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := s2.Pages.(*pagestore.InMemoryStore).Len(); n != 0 {
		t.Fatalf("incognito chat stored %d pages", n)
	}
}

func TestWebLoopLimits(t *testing.T) {
	var searches atomic.Int32
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if body["plugins"] != nil {
			searches.Add(1)
			return 200, []string{searchHelperReply(1)}
		}
		if body["tools"] == nil {
			return 200, []string{textChunk("final answer"), usageChunk}
		}
		// A model that never stops searching.
		n := len(toolMessages(body))
		return 200, []string{toolCallChunk([3]string{fmt.Sprintf("c%d", n), "web_search", fmt.Sprintf(`{"query":"q%d"}`, n)}), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "dig", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "auto"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if searches.Load() != webMaxSearches {
		t.Errorf("searches run = %d, want the limit %d", searches.Load(), webMaxSearches)
	}
	main := vendor.mainCalls()
	if len(main) != webMaxRounds {
		t.Fatalf("model calls = %d, want %d", len(main), webMaxRounds)
	}
	if main[len(main)-1]["tools"] != nil {
		t.Error("the last call still offered tools")
	}
	if text := toolMessages(main[len(main)-1])[webMaxSearches]["content"].(string); !strings.HasPrefix(text, "Search limit for this answer reached") {
		t.Errorf("over-limit search answered %q", text)
	}
	if done := events["done"][0].(chatResponse); done.ResponseText != "\n\nfinal answer" && done.ResponseText != "final answer" || len(done.Activity) != webMaxSearches {
		t.Errorf("done = %q with %d steps", done.ResponseText, len(done.Activity))
	}
}

func TestWebModesShapeTheRequest(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		return 200, []string{textChunk("ok"), `{"choices":[{"delta":{"annotations":[{"type":"url_citation","url_citation":{"url":"https://a.example","title":"A","content":"text"}}]}}]}`, usageChunk}
	}}
	s := newWebServer(t, vendor)
	ask := func(model, mode string) (map[string]any, chatResponse) {
		t.Helper()
		req := chatRequest{UserID: "u1", PlanID: "pro", Message: "новости дня", RequestedMode: "manual", ManualModelID: model, providerKey: "pza_user", WebSearch: mode}
		events, err := collectStream(s, context.Background(), req)
		if err != nil {
			t.Fatalf("%s/%s: %v", model, mode, err)
		}
		calls := vendor.calls()
		return calls[len(calls)-1], events["done"][0].(chatResponse)
	}

	if body, _ := ask("gpt-6-luna", "on"); body["tools"] == nil || !hasSystemMessage(body, webSearchInstruction) {
		t.Errorf("on with a tool model: tools %v, instruction %v", body["tools"], hasSystemMessage(body, webSearchInstruction))
	}
	if body, _ := ask("gpt-6-luna", "auto"); body["tools"] == nil || hasSystemMessage(body, webSearchInstruction) {
		t.Errorf("auto with a tool model: tools %v, instruction %v", body["tools"], hasSystemMessage(body, webSearchInstruction))
	}
	for _, mode := range []string{"off", ""} {
		body, _ := ask("gpt-6-luna", mode)
		if names := toolNames(body); len(names) != 1 || names[0] != "ask_user" || body["plugins"] != nil || hasSystemPrefix(body, "You can search the web") {
			t.Errorf("mode %q: tools %v, plugins %v (want only ask_user, no web)", mode, names, body["plugins"])
		}
	}
	if names := toolNames(func() map[string]any { b, _ := ask("gpt-6-luna", "auto"); return b }()); strings.Join(names, ",") != "web_search,web_fetch,ask_user" {
		t.Errorf("auto tools = %v", names)
	}
	if body, _ := ask("deepseek-text", "auto"); body["tools"] != nil || body["plugins"] != nil || !hasSystemMessage(body, noWebToolsNote) {
		t.Errorf("auto without tool calling: %v", body)
	}
	body, done := ask("deepseek-text", "on")
	plugin, _ := body["plugins"].([]any)
	if body["tools"] != nil || len(plugin) != 1 || plugin[0].(map[string]any)["search_prompt"] != "новости дня" || !hasSystemMessage(body, webPluginNote) {
		t.Errorf("on without tool calling: %v", body)
	}
	if len(done.Activity) != 1 || done.Activity[0].Query != "новости дня" || len(done.Activity[0].Results) != 1 || len(done.Sources) != 0 {
		t.Errorf("search-first activity = %+v, sources %+v", done.Activity, done.Sources)
	}
}

func TestWebToolsRefusedAnswersWithoutThem(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if body["tools"] != nil || body["plugins"] != nil {
			return http.StatusBadRequest, []string{`{"error":{"code":"BAD_REQUEST","message":"tools are not supported"}}`}
		}
		return 200, []string{textChunk("plain answer"), usageChunk}
	}}
	s := newWebServer(t, vendor)
	for _, model := range []string{"gpt-6-luna", "deepseek-text"} {
		req := chatRequest{UserID: "u1", PlanID: "pro", Message: "news?", RequestedMode: "manual", ManualModelID: model, providerKey: "pza_user", WebSearch: "on"}
		events, err := collectStream(s, context.Background(), req)
		if err != nil {
			t.Fatalf("%s: a vendor refusing web access broke the chat: %v", model, err)
		}
		done := events["done"][0].(chatResponse)
		if done.ResponseText != "plain answer" || len(done.Activity) != 1 || !strings.Contains(done.Activity[0].Error, "HTTP 400") || !strings.Contains(done.Activity[0].Error, "tools are not supported") {
			t.Errorf("%s: done = %q %+v", model, done.ResponseText, done.Activity)
		}
		calls := vendor.calls()
		retry := calls[len(calls)-1]
		if retry["tools"] != nil || retry["plugins"] != nil || !hasSystemMessage(retry, webUnavailableNote) || hasSystemMessage(retry, webSearchInstruction) || hasSystemPrefix(retry, "You can search the web") {
			t.Errorf("%s: retry = %v", model, retry)
		}
	}
}

func TestWebLoopCarriesOnWhenReasoningCantComeBack(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if body["plugins"] != nil {
			return 200, []string{searchHelperReply(2)}
		}
		if len(toolMessages(body)) == 0 {
			return 200, []string{
				`{"choices":[{"delta":{"reasoning":"think","reasoning_details":[{"type":"reasoning.text","text":"think","signature":"sig"}]}}]}`,
				toolCallChunk([3]string{"s1", "web_search", `{"query":"x"}`}),
				usageChunk,
			}
		}
		for _, m := range body["messages"].([]any) {
			if m.(map[string]any)["reasoning_details"] != nil {
				return http.StatusBadRequest, []string{`{"error":{"message":"unexpected reasoning_details"}}`}
			}
		}
		return 200, []string{textChunk("answer"), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "q", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "auto", ReasoningEffort: "high"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if done := events["done"][0].(chatResponse); done.ResponseText != "answer" {
		t.Fatalf("response = %q", done.ResponseText)
	}
	main := vendor.mainCalls()
	if len(main) != 3 {
		t.Fatalf("main calls = %d, want tool call, refused continuation, retry", len(main))
	}
	if r := main[2]["reasoning"].(map[string]any); r["effort"] != "none" {
		t.Errorf("retry reasoning = %v, want off", r)
	}
	if r := main[1]["reasoning"].(map[string]any); r["effort"] != "high" {
		t.Errorf("first continuation reasoning = %v, want the user's", r)
	}
}

func TestMergeSearchResults(t *testing.T) {
	citations := []provider.WebResult{{URL: "https://a.example", Title: "A"}, {URL: "javascript:alert(1)"}, {URL: "https://a.example"}, {URL: " https://b.example "}}
	copied := []provider.WebResult{{URL: "https://a.example", Title: "A copy", Snippet: "  about\n a  "}, {URL: "https://b.example", Title: "B", Snippet: "b"}, {URL: "https://made-up.example"}}
	got := mergeSearchResults(citations, copied)
	if len(got) != 2 || got[0].Title != "A" || got[0].Snippet != "about a" || got[1].URL != "https://b.example" || got[1].Title != "B" {
		t.Fatalf("merged = %+v", got)
	}
	if got := mergeSearchResults(nil, copied); len(got) != 3 {
		t.Fatalf("copy alone = %+v", got)
	}
	if got := parseSearchJSON("```json\n{\"results\":[{\"title\":\"T\",\"url\":\"https://t.example\"}]}\n```"); len(got) != 1 || got[0].URL != "https://t.example" {
		t.Fatalf("fenced JSON = %+v", got)
	}
	if parseSearchJSON("no json here") != nil {
		t.Fatal("garbage parsed")
	}
}

func TestFinishedStepsMarksStoppedOnes(t *testing.T) {
	got := finishedSteps([]conversation.ToolActivity{{Tool: "web_fetch", URL: "https://a.example", Pending: true}, {Tool: "web_search", Query: "q", Results: []conversation.WebLink{{URL: "https://b.example"}}}})
	if got[0].Pending || got[0].Error != "stopped" || got[1].Error != "" {
		t.Fatalf("steps = %+v", got)
	}
}

func TestCleanupSweepsStoredPages(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{})
	pages := pagestore.NewInMemoryStore()
	s.Pages = pages
	ctx := context.Background()
	_ = pages.Put(ctx, pagestore.Page{UserID: "u1", ConversationID: "c1", URL: "https://old.example", Text: "x", FetchedAt: time.Now().Add(-25 * time.Hour)})
	_ = pages.Put(ctx, pagestore.Page{UserID: "u1", ConversationID: "c1", URL: "https://new.example", Text: "x", FetchedAt: time.Now()})
	s.cleanupOnce(ctx)
	if pages.Len() != 1 {
		t.Fatalf("pages after cleanup = %d, want the fresh one", pages.Len())
	}
}

func TestPlainExcerpt(t *testing.T) {
	text := "# Release notes\n\nWe shipped [the docs](https://x.dev/docs) and `go get`.\n- Faster sync\n- Dark mode\n```\ncode\n```"
	if got := plainExcerpt(text, 280); got != "Release notes We shipped the docs and go get. Faster sync Dark mode code" {
		t.Fatalf("excerpt = %q", got)
	}
	if got := plainExcerpt(strings.Repeat("слово ", 100), 20); got != "слово слово слово сл…" {
		t.Fatalf("clipped excerpt = %q", got)
	}
}

func TestWebLoopFallsBackToTextWhenToolResultsAreRefused(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if body["plugins"] != nil {
			return 200, []string{searchHelperReply(3)}
		}
		if len(toolMessages(body)) > 0 {
			return http.StatusBadRequest, []string{`{"error":{"message":"function calling turns are not supported on this route"}}`}
		}
		if body["tools"] != nil {
			return 200, []string{textChunk("Checking."), toolCallChunk([3]string{"s1", "web_search", `{"query":"x"}`}), usageChunk}
		}
		return 200, []string{textChunk("From the results: item 1."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "q", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "auto"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if done := events["done"][0].(chatResponse); done.ResponseText != "Checking.\n\nFrom the results: item 1." || len(done.Activity) != 1 {
		t.Fatalf("done = %q, %+v", done.ResponseText, done.Activity)
	}
	main := vendor.mainCalls()
	final := main[len(main)-1]
	if final["tools"] != nil || len(toolMessages(final)) != 0 || !hasSystemPrefix(final, "Results of the web searches and pages you asked for") {
		t.Fatalf("final call = %v", final)
	}
	for _, m := range final["messages"].([]any) {
		if m.(map[string]any)["tool_calls"] != nil {
			t.Fatal("final call still carries tool calls")
		}
	}
}
