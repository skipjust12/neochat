package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"neochat/conversation"
)

func TestCleanQuote(t *testing.T) {
	for in, want := range map[string]string{
		"  утечка  ":                             "утечка",
		"first line  \r\n\r\n\r\n\r\nsecond\t\n": "first line\n\nsecond",
		"a\rb":                                   "a\nb",
		"\n\n":                                   "",
	} {
		if got := cleanQuote(in); got != want {
			t.Errorf("cleanQuote(%q) = %q, want %q", in, got, want)
		}
	}
	if got := []rune(cleanQuote(strings.Repeat("ж", maxQuoteRunes+50))); len(got) != maxQuoteRunes+1 || got[maxQuoteRunes] != '…' {
		t.Errorf("long quote kept %d runes", len(got))
	}
}

func TestWithQuote(t *testing.T) {
	if got := withQuote("", "hi"); got != "hi" {
		t.Errorf("no quote: %q", got)
	}
	if got := withQuote("one\n\ntwo", "what?"); got != "> one\n>\n> two\n\nwhat?" {
		t.Errorf("quote: %q", got)
	}
	if got := withQuote("only", " "); got != "> only" {
		t.Errorf("quote without text: %q", got)
	}
}

// postStream sends body to path through the real mux and returns the SSE
// transcript.
func postStream(t *testing.T, mux http.Handler, path, token string, body map[string]any) string {
	t.Helper()
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(providerKeyHeader, "pza_user")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "event: done") {
		t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
	}
	return response.Body.String()
}

func doneConversationID(t *testing.T, transcript string) string {
	t.Helper()
	for _, block := range strings.Split(transcript, "\n\n") {
		if strings.HasPrefix(block, "event: done\n") {
			var done chatResponse
			if err := json.Unmarshal([]byte(strings.TrimPrefix(block, "event: done\ndata: ")), &done); err != nil {
				t.Fatal(err)
			}
			return done.ConversationID
		}
	}
	t.Fatalf("no done event in %s", transcript)
	return ""
}

func userContents(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, m := range body["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "user" {
			text, _ := msg["content"].(string)
			out = append(out, text)
		}
	}
	return out
}

func TestQuoteGoesToTheModelAndStaysWithTheMessage(t *testing.T) {
	standIn := &polzaStandIn{reply: "answer"}
	s := newDirectServer(t, standIn)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	turn := map[string]any{"requested_mode": "manual", "manual_model_id": "gpt-6-luna", "web_search": "off"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range turn {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	first := postStream(t, mux, "/chat/stream", token, with(map[string]any{"message": "start"}))
	conversationID := doneConversationID(t, first)

	postStream(t, mux, "/chat/stream", token, with(map[string]any{"conversation_id": conversationID, "message": "что это значит?", "quote": "  утечка памяти  \r\n\r\n\r\nв горутинах "}))
	want := "> утечка памяти\n>\n> в горутинах\n\nчто это значит?"
	if got := lastUserContent(t, standIn); got != want {
		t.Fatalf("model got %q, want %q", got, want)
	}

	history, err := s.Conversations.History(context.Background(), "u1", conversationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if asked := history[2]; asked.Role != conversation.RoleUser || asked.Content != "что это значит?" || asked.Quote != "утечка памяти\n\nв горутинах" {
		t.Fatalf("stored message = %+v", asked)
	}

	// Regenerating the answer asks about the same quote.
	postStream(t, mux, "/chat/regenerate/stream", token, with(map[string]any{"conversation_id": conversationID}))
	if got := lastUserContent(t, standIn); got != want {
		t.Fatalf("regenerated request got %q", got)
	}

	// Later turns still see what the earlier question was about.
	postStream(t, mux, "/chat/stream", token, with(map[string]any{"conversation_id": conversationID, "message": "а подробнее?"}))
	if users := userContents(t, standIn.lastBody(t)); len(users) != 3 || users[1] != want || users[2] != "а подробнее?" {
		t.Fatalf("history user turns = %q", users)
	}

	// The thread comes back with the quote for the UI to show.
	request := httptest.NewRequest(http.MethodGet, "/conversations/"+conversationID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"quote":"утечка памяти\n\nв горутинах"`) {
		t.Fatalf("history response lacks the quote: %s", response.Body.String())
	}
}

func TestQuoteNeedsAMessageAndWorksInIncognito(t *testing.T) {
	standIn := &polzaStandIn{reply: "ok"}
	s := newDirectServer(t, standIn)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")

	encoded, _ := json.Marshal(map[string]any{"message": "", "quote": "утечка", "requested_mode": "manual", "manual_model_id": "gpt-6-luna"})
	request := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("quote without a message: %d, want 400", response.Code)
	}

	postStream(t, mux, "/chat/stream", token, map[string]any{
		"message": "и?", "quote": "второй", "requested_mode": "manual", "manual_model_id": "gpt-6-luna", "web_search": "off", "incognito": true,
		"incognito_history": []map[string]any{
			{"role": "user", "content": "почему?", "quote": " первый "},
			{"role": "assistant", "content": "потому что"},
		},
	})
	if users := userContents(t, standIn.lastBody(t)); len(users) != 2 || users[0] != "> первый\n\nпочему?" || users[1] != "> второй\n\nи?" {
		t.Fatalf("incognito user turns = %q", users)
	}
}
