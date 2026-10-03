package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"neochat/limits"
	"neochat/provider"
)

var searchChunks = []string{
	`{"choices":[],"polza":{"type":"tool_call","id":"s1","tool":"polza:web_search","arguments":{"query":"anthropic claude pricing"}}}`,
	`{"choices":[],"polza":{"type":"tool_result","id":"s1","tool":"polza:web_search","result":[{"title":"Pricing","url":"https://www.anthropic.com/pricing","snippet":"Opus 5.5 $4 / MTok"},{"title":"Models","url":"https://docs.anthropic.com/models"}]}}`,
	`{"choices":[],"polza":{"type":"tool_call","id":"f1","tool":"polza:web_fetch","arguments":{"url":"https://www.anthropic.com/pricing"}}}`,
	`{"choices":[],"polza":{"type":"tool_result","id":"f1","tool":"polza:web_fetch","result":{"title":"Claude pricing","content":"..."}}}`,
	`{"choices":[],"polza":{"type":"tool_call","id":"d1","tool":"polza:datetime","arguments":{}}}`,
	`{"choices":[{"delta":{"annotations":[{"type":"url_citation","url_citation":{"url":"https://www.anthropic.com/pricing","title":"Pricing"}}]}}]}`,
}

func TestWebSearchModesShapeTheVendorRequest(t *testing.T) {
	standIn := &polzaStandIn{reply: "ok"}
	s := newDirectServer(t, standIn)
	ask := func(model, mode string) map[string]any {
		t.Helper()
		req := chatRequest{UserID: "u1", PlanID: "pro", Message: "news?", RequestedMode: "manual", ManualModelID: model, providerKey: "pza_user", WebSearch: mode}
		if _, err := collectStream(s, context.Background(), req); err != nil {
			t.Fatalf("%s/%s: %v", model, mode, err)
		}
		return standIn.lastBody(t)
	}
	hasInstruction := func(body map[string]any) bool {
		for _, m := range body["messages"].([]any) {
			if content, _ := m.(map[string]any)["content"].(string); content == webSearchInstruction {
				return true
			}
		}
		return false
	}

	if body := ask("gpt-6-luna", "auto"); body["tools"] == nil || hasInstruction(body) {
		t.Errorf("auto on a tool model: tools=%v instruction=%v", body["tools"], hasInstruction(body))
	}
	if body := ask("gpt-6-luna", "on"); body["tools"] == nil || !hasInstruction(body) {
		t.Errorf("on with a tool model must offer tools and ask to search: %v", body["tools"])
	}
	if body := ask("deepseek-text", "auto"); body["tools"] != nil || body["web_search_options"] != nil {
		t.Errorf("auto on a model without tools must not search: %v", body)
	}
	if body := ask("deepseek-text", "on"); body["tools"] != nil || body["web_search_options"] == nil {
		t.Errorf("on without tools must search before the model: %v", body)
	}
	for _, mode := range []string{"off", ""} {
		if body := ask("gpt-6-luna", mode); body["tools"] != nil || body["web_search_options"] != nil {
			t.Errorf("mode %q sent web settings: %v", mode, body)
		}
	}
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "x", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "always"}
	if _, err := collectStream(s, context.Background(), req); err == nil {
		t.Fatal("an unknown web_search mode was accepted")
	}
}

func TestWebSearchActivityStreamsAndPersists(t *testing.T) {
	standIn := &polzaStandIn{reply: "Opus 5.5 costs $4 per million input tokens.", chunks: searchChunks}
	s := newDirectServer(t, standIn)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "how much is opus?", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "auto"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(events["activity"]) != 5 {
		t.Fatalf("activity events = %d, want one per tool event (5)", len(events["activity"]))
	}
	done := events["done"][0].(chatResponse)
	if len(done.Activity) != 2 {
		t.Fatalf("activity = %+v, want a search and a fetch (datetime hidden)", done.Activity)
	}
	search, fetch := done.Activity[0], done.Activity[1]
	if search.Tool != "web_search" || search.Query != "anthropic claude pricing" || len(search.Results) != 2 || search.Results[0].URL != "https://www.anthropic.com/pricing" {
		t.Errorf("search = %+v", search)
	}
	if fetch.Tool != "web_fetch" || fetch.URL != "https://www.anthropic.com/pricing" || fetch.Title != "Claude pricing" {
		t.Errorf("fetch = %+v", fetch)
	}
	if len(done.Sources) != 1 || done.Sources[0].Title != "Pricing" {
		t.Errorf("sources = %+v", done.Sources)
	}

	history, _ := s.Conversations.History(context.Background(), "u1", done.ConversationID, 0)
	stored := history[1].Versions
	if len(stored) != 1 || len(stored[0].Activity) != 2 || len(stored[0].Sources) != 1 {
		t.Fatalf("stored versions = %+v", stored)
	}
}

func TestWebSearchFeesAreCharged(t *testing.T) {
	usage := `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":10,"server_tool_use":{"web_search_requests":3}}}`
	standIn := &polzaStandIn{reply: "ok", chunks: []string{usage}}
	s := newDirectServer(t, standIn)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "x", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "auto"}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	spent, _ := s.Store.Sum(context.Background(), "u1", limits.PoolInstant, time.Hour)
	if spent < 3*webSearchFeeUSD {
		t.Fatalf("spent %.6f, want at least the three searches (%.6f)", spent, 3*webSearchFeeUSD)
	}
}

func TestWebSearchRejectedByVendorFallsBackToPlainAnswer(t *testing.T) {
	standIn := &polzaStandIn{reply: "plain answer", rejectWeb: true}
	s := newDirectServer(t, standIn)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "news?", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "on"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatalf("a vendor refusing web tools broke the chat: %v", err)
	}
	if got := events["done"][0].(chatResponse).ResponseText; got != "plain answer" {
		t.Fatalf("response = %q", got)
	}
	if len(standIn.bodies) != 2 {
		t.Fatalf("vendor calls = %d, want the refused one and one retry", len(standIn.bodies))
	}
	retry := standIn.lastBody(t)
	if retry["tools"] != nil || retry["web_search_options"] != nil {
		t.Fatalf("retry still asked for web access: %v", retry)
	}
	if hasSystemMessage(retry, webSearchInstruction) {
		t.Fatal("retry without tools still tells the model to search")
	}
	if !hasSystemMessage(retry, webUnavailableNote) {
		t.Fatal("retry without tools doesn't tell the model search failed")
	}
	if spent, _ := s.Store.Sum(context.Background(), "u1", limits.PoolInstant, time.Hour); spent <= 0 || spent >= webSearchFeeUSD {
		t.Fatalf("spent %.6f: want the plain answer billed, the refused call free", spent)
	}

	// No provider supporting the tools (require_parameters routing) is a
	// 404, and falls back the same way.
	noRoute := &polzaStandIn{reply: "plain answer", rejectWeb: true, rejectStatus: http.StatusNotFound}
	s = newDirectServer(t, noRoute)
	if _, err := collectStream(s, context.Background(), req); err != nil || len(noRoute.bodies) != 2 {
		t.Fatalf("404 for web tools: err=%v calls=%d, want a retry that succeeds", err, len(noRoute.bodies))
	}

	// A refusal that has nothing to do with web tools isn't retried.
	plain := &polzaStandIn{status: http.StatusBadRequest, errBody: `{"error":{"message":"bad"}}`}
	s = newDirectServer(t, plain)
	req.WebSearch = "off"
	if _, err := collectStream(s, context.Background(), req); err == nil {
		t.Fatal("a 400 without web tools succeeded")
	}
	if len(plain.bodies) != 1 {
		t.Fatalf("vendor calls = %d, want 1", len(plain.bodies))
	}
}

func TestAutoModeExplainsMissingWebAccess(t *testing.T) {
	standIn := &polzaStandIn{reply: "ok"}
	s := newDirectServer(t, standIn)
	ask := func(model, mode string) map[string]any {
		t.Helper()
		req := chatRequest{UserID: "u1", PlanID: "pro", Message: "search for the latest openai model", RequestedMode: "manual", ManualModelID: model, providerKey: "pza_user", WebSearch: mode}
		if _, err := collectStream(s, context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return standIn.lastBody(t)
	}
	body := ask("deepseek-text", "auto")
	if !hasSystemMessage(body, noWebToolsNote) {
		t.Fatal("a model that can't search, on Auto, isn't told so")
	}
	// The note sits with the other system messages, before the conversation.
	messages := body["messages"].([]any)
	if last := messages[len(messages)-1].(map[string]any); last["role"] != "user" {
		t.Fatalf("last message = %v, want the user's", last)
	}
	for _, m := range messages[:len(messages)-1] {
		if m.(map[string]any)["role"] != "system" {
			t.Fatalf("messages = %v, want the note among the leading system messages", messages)
		}
	}
	for _, c := range []struct{ model, mode string }{{"gpt-6-luna", "auto"}, {"deepseek-text", "on"}, {"deepseek-text", "off"}} {
		if hasSystemMessage(ask(c.model, c.mode), noWebToolsNote) {
			t.Errorf("%s/%s got the no-web-tools note", c.model, c.mode)
		}
	}
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

func TestBuildActivity(t *testing.T) {
	got := buildActivity([]provider.ToolEvent{
		{Tool: "web_search", Phase: "call", Query: "a"},
		{Tool: "datetime", Phase: "call"},
		{Tool: "web_search", Phase: "call", ID: "x", Query: "b"},
		{Phase: "result", ID: "x", Results: []provider.WebResult{{URL: "https://b"}}},
		{Tool: "web_search", Phase: "result", Results: []provider.WebResult{{URL: "https://a"}}},
		{Phase: "result", ID: "unknown", Results: []provider.WebResult{{URL: "https://lost"}}},
	})
	if len(got) != 2 || got[0].Query != "a" || got[0].Results[0].URL != "https://a" || got[1].Query != "b" || got[1].Results[0].URL != "https://b" {
		t.Fatalf("buildActivity = %+v", got)
	}
	if strings.Contains(strings.Join([]string{got[0].Query, got[1].Query}, ""), "lost") {
		t.Fatal("an unmatched result was attached somewhere")
	}
}
