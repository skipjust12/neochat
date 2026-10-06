package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"neochat/attachment"
	"neochat/conversation"
	"neochat/costlog"
	"neochat/provider"
)

// titleStandIn answers the naming calls (system prompt titleSystemPrompt)
// with title, once release is closed (if set); everything else goes to
// chat.
type titleStandIn struct {
	chat    *polzaStandIn
	title   string
	release chan struct{}

	mu     sync.Mutex
	inputs []string
	bodies []map[string]any
	keys   []string
}

func (s *titleStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	messages, _ := body["messages"].([]any)
	if len(messages) == 2 {
		first, _ := messages[0].(map[string]any)
		if content, _ := first["content"].(string); content == titleSystemPrompt {
			second, _ := messages[1].(map[string]any)
			input, _ := second["content"].(string)
			s.mu.Lock()
			s.inputs = append(s.inputs, input)
			s.bodies = append(s.bodies, body)
			s.keys = append(s.keys, r.Header.Get("Authorization"))
			s.mu.Unlock()
			if s.release != nil {
				select {
				case <-s.release:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":120,"completion_tokens":6}}`, s.title)
			return
		}
	}
	// Not a naming call: hand the same body to the chat stand-in.
	encoded, _ := json.Marshal(body)
	forward := httptest.NewRequest(r.Method, r.URL.Path, strings.NewReader(string(encoded)))
	forward.Header = r.Header.Clone()
	s.chat.ServeHTTP(w, forward)
}

func (s *titleStandIn) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.inputs...)
}

func newTitleServer(t *testing.T, title string) (*Server, *titleStandIn) {
	t.Helper()
	chat := &polzaStandIn{reply: "Борщ варят так."}
	s := newDirectServer(t, chat)
	standIn := &titleStandIn{chat: chat, title: title}
	vendor := httptest.NewServer(standIn)
	t.Cleanup(vendor.Close)
	client := provider.NewPolzaClient("")
	client.BaseURL = vendor.URL
	for name := range s.Generators {
		s.Generators[name] = client
	}
	s.TitleModelID = DefaultTitleModelID
	return s, standIn
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func listedTitle(t *testing.T, s *Server, userID, conversationID string) string {
	t.Helper()
	list, err := s.Conversations.List(context.Background(), userID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range list {
		if o.ID == conversationID {
			return o.Title
		}
	}
	return ""
}

func TestNewChatIsNamedByTheTitleModel(t *testing.T) {
	s, standIn := newTitleServer(t, `Title: "Рецепт борща."`)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	events, _, _, ok := s.streams.watch("u1")
	if !ok {
		t.Fatal("watch refused")
	}
	defer s.streams.unwatch("u1", events)
	key := map[string]string{providerKeyHeader: "pza_chat"}

	first := doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, map[string]any{
		"message": "как сварить борщ на 6 человек?", "requested_mode": "manual", "manual_model_id": "deepseek-text",
	}))
	waitFor(t, "the generated name", func() bool {
		return listedTitle(t, s, "u1", first.ConversationID) == "Рецепт борща"
	})
	inputs := standIn.calls()
	if len(inputs) != 1 || !strings.Contains(inputs[0], "<message>\nкак сварить борщ на 6 человек?\n</message>") {
		t.Fatalf("naming input = %q", inputs)
	}
	standIn.mu.Lock()
	body, auth := standIn.bodies[0], standIn.keys[0]
	standIn.mu.Unlock()
	if body["model"] != "openai/gpt-6-luna" || auth != "Bearer pza_chat" || body["stream"] == true {
		t.Fatalf("naming call: model %v, auth %q, stream %v", body["model"], auth, body["stream"])
	}
	// Everyone open hears the new name.
	var heard liveNotice
	waitFor(t, "the title event", func() bool {
		for {
			select {
			case event := <-events:
				if event.name == "title" {
					heard = event.notice
					return true
				}
			default:
				return false
			}
		}
	})
	if heard.ConversationID != first.ConversationID || heard.Title != "Рецепт борща" {
		t.Fatalf("title event = %+v", heard)
	}
	// Billed and logged like a helper call.
	var logged bool
	for _, entry := range s.CostLog.(*costlog.InMemoryStore).Entries() {
		if entry.Mode == "title" && entry.UserID == "u1" && entry.InputTokens == 120 && entry.OutputTokens == 6 {
			logged = true
		}
	}
	if !logged {
		t.Fatal("the naming call isn't in the cost log")
	}

	// A follow-up doesn't rename the chat.
	doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, map[string]any{
		"conversation_id": first.ConversationID, "message": "а без мяса?", "requested_mode": "manual", "manual_model_id": "deepseek-text",
	}))
	time.Sleep(50 * time.Millisecond)
	if n := len(standIn.calls()); n != 1 {
		t.Fatalf("naming calls after a follow-up = %d, want 1", n)
	}
}

// A first message whose answer failed leaves the client holding the
// chat's id; the retry, sent with that id, still opens the chat and names
// it.
func TestRetryAfterAFailedFirstAnswerStillNamesTheChat(t *testing.T) {
	s, standIn := newTitleServer(t, "Рецепт борща")
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	key := map[string]string{providerKeyHeader: "pza_chat"}

	standIn.chat.status, standIn.chat.errBody = http.StatusBadGateway, `{"error":{"message":"upstream down"}}`
	failed := sendStream(t, mux, "/chat/stream", token, key, map[string]any{
		"message": "как сварить борщ?", "requested_mode": "manual", "manual_model_id": "deepseek-text",
	})
	var id string
	for _, event := range parseSSE(failed) {
		if event.name == "meta" {
			var meta map[string]any
			_ = json.Unmarshal([]byte(event.data), &meta)
			id, _ = meta["conversation_id"].(string)
		}
	}
	if id == "" {
		t.Fatalf("no conversation id in %s", failed)
	}
	if list, _ := s.Conversations.List(context.Background(), "u1", 10); len(list) != 0 {
		t.Fatalf("a failed first answer stored the chat: %+v", list)
	}

	standIn.chat.status = 0
	doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, map[string]any{
		"conversation_id": id, "message": "как сварить борщ?", "requested_mode": "manual", "manual_model_id": "deepseek-text",
	}))
	waitFor(t, "the name", func() bool { return listedTitle(t, s, "u1", id) == "Рецепт борща" })
	if n := len(standIn.calls()); n != 2 {
		t.Fatalf("naming calls = %d, want 2 (one per attempt)", n)
	}
}

func TestGeneratedNameNeverReplacesTheUsersName(t *testing.T) {
	s, standIn := newTitleServer(t, "Рецепт борща")
	standIn.release = make(chan struct{})
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	key := map[string]string{providerKeyHeader: "pza_chat"}

	first := doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, map[string]any{
		"message": "как сварить борщ?", "requested_mode": "manual", "manual_model_id": "deepseek-text",
	}))
	// The user names the chat while the model is still thinking of one.
	mine := "Суп на выходные"
	if err := s.Conversations.UpdateMetadata(context.Background(), "u1", first.ConversationID, conversation.MetadataUpdate{Title: &mine}); err != nil {
		t.Fatal(err)
	}
	close(standIn.release)
	waitFor(t, "the naming call", func() bool { return len(standIn.calls()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := listedTitle(t, s, "u1", first.ConversationID); got != mine {
		t.Fatalf("title = %q, want the user's %q", got, mine)
	}
}

func TestNothingToNameWithoutASavedChatOrAChatKey(t *testing.T) {
	s, standIn := newTitleServer(t, "Name")
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")

	// Incognito chats are never stored, so never named.
	doneEvent(t, sendStream(t, mux, "/chat/stream", token, map[string]string{providerKeyHeader: "pza_chat"}, map[string]any{
		"message": "секрет", "incognito": true, "requested_mode": "manual", "manual_model_id": "deepseek-text",
	}))
	// Naming switched off.
	s.TitleModelID = ""
	doneEvent(t, sendStream(t, mux, "/chat/stream", token, map[string]string{providerKeyHeader: "pza_chat"}, map[string]any{
		"message": "привет", "requested_mode": "manual", "manual_model_id": "deepseek-text",
	}))
	time.Sleep(50 * time.Millisecond)
	if n := len(standIn.calls()); n != 0 {
		t.Fatalf("naming calls = %d, want 0", n)
	}
}

func TestChatTitlerAppliesOnceWhicheverComesLast(t *testing.T) {
	for _, nameFirst := range []bool{true, false} {
		s := &Server{Conversations: conversation.NewInMemoryStore()}
		ctx := context.Background()
		if err := s.Conversations.Append(ctx, "u1", "c1", conversation.Message{Role: conversation.RoleUser, Content: "hi", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		events, _, _, _ := s.streams.watch("u1")
		titler := &chatTitler{server: s, userID: "u1", conversationID: "c1"}
		if nameFirst {
			titler.nameReady("Greeting")
			if got := listedTitle(t, s, "u1", "c1"); got != "hi" {
				t.Fatalf("named before the chat was stored: %q", got)
			}
			titler.turnStored()
		} else {
			titler.turnStored()
			titler.nameReady("Greeting")
		}
		titler.turnStored()
		titler.nameReady("Other")
		if got := listedTitle(t, s, "u1", "c1"); got != "Greeting" {
			t.Fatalf("nameFirst=%v: title = %q", nameFirst, got)
		}
		if n := len(events); n != 1 {
			t.Fatalf("nameFirst=%v: %d events, want 1", nameFirst, n)
		}
	}
	// A failed naming leaves the first-message name.
	var nilTitler *chatTitler
	nilTitler.turnStored()
	nilTitler.nameReady("x")
}

func TestCleanTitle(t *testing.T) {
	cases := map[string]string{
		`"Рецепт борща."`:                      "Рецепт борща",
		"Title: Fixing a Go race\nExplanation": "Fixing a Go race",
		"**Trip plan for Kyoto**":              "Trip plan for Kyoto",
		"Название: «Письмо арендодателю»":      "Письмо арендодателю",
		"  spaced    out   name  ":             "spaced out name",
		`""`:                                   "",
		strings.Repeat("слово ", 20):           strings.TrimSpace(string([]rune(strings.Repeat("слово ", 20))[:maxTitleRunes])),
	}
	for in, want := range cases {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitleInputCarriesFileNames(t *testing.T) {
	got := titleInput("", nil)
	if got != "" {
		t.Fatalf("empty input = %q", got)
	}
	files := []attachment.File{{Name: "lease.pdf"}, {Name: "photo.webp"}}
	if got := titleInput("", files); got != "<message>\n\n</message>\nAttached files: lease.pdf, photo.webp" {
		t.Fatalf("files only = %q", got)
	}
	long := strings.Repeat("я", maxTitleInput+50)
	if got := titleInput(long, nil); !strings.Contains(got, strings.Repeat("я", maxTitleInput)+"…") || strings.Contains(got, strings.Repeat("я", maxTitleInput+1)) {
		t.Fatal("long message not clipped")
	}
}

func TestTheAskingDeviceHearsTheNameWhileTheAnswerStreams(t *testing.T) {
	s, standIn := newTitleServer(t, "Рецепт борща")
	standIn.chat.holdOpen = make(chan struct{})
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	transcript := make(chan string, 1)
	go func() {
		transcript <- sendStream(t, mux, "/chat/stream", token, map[string]string{providerKeyHeader: "pza_chat"}, map[string]any{
			"message": "как сварить борщ?", "requested_mode": "manual", "manual_model_id": "deepseek-text",
		})
	}()
	waitFor(t, "the naming call", func() bool { return len(standIn.calls()) == 1 })
	time.Sleep(50 * time.Millisecond)
	close(standIn.chat.holdOpen)
	events := parseSSE(<-transcript)
	var titleAt, doneAt = -1, -1
	var named map[string]string
	for i, event := range events {
		switch event.name {
		case "title":
			titleAt = i
			_ = json.Unmarshal([]byte(event.data), &named)
		case "done":
			doneAt = i
		}
	}
	if titleAt < 0 || doneAt < 0 || titleAt > doneAt || named["title"] != "Рецепт борща" || named["conversation_id"] == "" {
		t.Fatalf("title event at %d (%v), done at %d", titleAt, named, doneAt)
	}
}
