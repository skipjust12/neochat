package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"neochat/auth"
	"neochat/conversation"
	"neochat/settings"
)

func sendJSON(t *testing.T, srv *Server, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	srv.Mux().ServeHTTP(response, request)
	return response
}

func TestSettingsSync(t *testing.T) {
	authStore := auth.NewInMemoryStore()
	token, _ := authStore.IssueKey(context.Background(), "u1", "pro")
	other, _ := authStore.IssueKey(context.Background(), "u2", "pro")
	srv := &Server{Auth: authStore, Settings: settings.NewInMemoryStore()}

	if got := sendJSON(t, srv, "GET", "/account/settings", token, "", nil); got.Code != 200 || strings.TrimSpace(got.Body.String()) != "{}" {
		t.Fatalf("empty settings: %d %q", got.Code, got.Body)
	}
	saved := `{"tone":"Direct","theme":"dark"}`
	if got := sendJSON(t, srv, "PUT", "/account/settings", token, saved, nil); got.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", got.Code, got.Body)
	}
	if got := sendJSON(t, srv, "GET", "/account/settings", token, "", nil); got.Body.String() != saved {
		t.Fatalf("get = %q, want %q", got.Body, saved)
	}
	if got := sendJSON(t, srv, "GET", "/account/settings", other, "", nil); strings.TrimSpace(got.Body.String()) != "{}" {
		t.Fatalf("another user sees %q", got.Body)
	}
	for _, bad := range []string{`{"providerKey":"pza_x"}`, `[1]`, `null`, `not json`, `{"x":"` + strings.Repeat("a", 33<<10) + `"}`} {
		if got := sendJSON(t, srv, "PUT", "/account/settings", token, bad, nil); got.Code != http.StatusBadRequest {
			t.Fatalf("put %.30q: %d, want 400", bad, got.Code)
		}
	}
	if got := sendJSON(t, srv, "GET", "/account/settings", token, "", nil); got.Body.String() != saved {
		t.Fatalf("rejected puts changed settings to %q", got.Body)
	}
}

func TestConversationCompact(t *testing.T) {
	standIn := &polzaStandIn{reply: "The user and the assistant planned a trip."}
	s := newDirectServer(t, standIn)
	token, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u1", "pro")
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	add := func(role conversation.Role, text string, i int) {
		if err := s.Conversations.Append(ctx, "u1", "c1", conversation.Message{Role: role, Content: text, CreatedAt: at.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	add(conversation.RoleUser, "plan a trip", 0)
	add(conversation.RoleAssistant, "where to?", 1)
	keyHeader := map[string]string{providerKeyHeader: "pza_user"}

	// Two messages: nothing to fold yet.
	if got := sendJSON(t, s, "POST", "/conversations/c1/compact", token, "", keyHeader); got.Code != http.StatusConflict {
		t.Fatalf("short chat: %d %s", got.Code, got.Body)
	}
	if got := sendJSON(t, s, "POST", "/conversations/c1/compact", token, "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("no key: %d", got.Code)
	}
	if got := sendJSON(t, s, "POST", "/conversations/missing/compact", token, "", keyHeader); got.Code != http.StatusNotFound {
		t.Fatalf("missing chat: %d", got.Code)
	}

	for i := 2; i < 6; i += 2 {
		add(conversation.RoleUser, fmt.Sprintf("question %d", i), i)
		add(conversation.RoleAssistant, fmt.Sprintf("answer %d", i), i+1)
	}
	got := sendJSON(t, s, "POST", "/conversations/c1/compact", token, "", keyHeader)
	if got.Code != 200 {
		t.Fatalf("compact: %d %s", got.Code, got.Body)
	}
	var compacted compactResponse
	if err := json.Unmarshal(got.Body.Bytes(), &compacted); err != nil {
		t.Fatal(err)
	}
	history, _ := s.Conversations.History(ctx, "u1", "c1", 0)
	if compacted.CompactedThroughID != history[3].ID {
		t.Fatalf("compacted through %d, want %d (all but the last exchange)", compacted.CompactedThroughID, history[3].ID)
	}
	state, _ := s.Conversations.GetSummary(ctx, "u1", "c1")
	if state.CoversThrough != 4 || state.Text != standIn.reply {
		t.Fatalf("summary = %+v", state)
	}
	body := standIn.lastBody(t)
	if body["model"] != "openai/gpt-6-luna" || standIn.auth[len(standIn.auth)-1] != "Bearer pza_user" {
		t.Fatalf("compacted with %v / %q, want luna on the user's key", body["model"], standIn.auth[len(standIn.auth)-1])
	}
	if text := fmt.Sprint(body["messages"]); !strings.Contains(text, "answer 2") || strings.Contains(text, "answer 4") {
		t.Fatalf("summarized %s, want everything but the last exchange", text)
	}

	var page conversationHistoryResponse
	if code := getJSON(t, s, "GET", "/conversations/c1", token, nil, &page); code != 200 || page.CompactedThroughID != history[3].ID {
		t.Fatalf("history: %d, compacted_through_id = %d", code, page.CompactedThroughID)
	}
	// Again right away: the tail is all that's left.
	if got := sendJSON(t, s, "POST", "/conversations/c1/compact", token, "", keyHeader); got.Code != http.StatusConflict {
		t.Fatalf("second compact: %d", got.Code)
	}
}
