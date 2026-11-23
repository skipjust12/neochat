package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"neochat/auth"
	"neochat/conversation"
	"neochat/costlog"
	"neochat/idempotency"
	"neochat/limits"
	"neochat/provider"
	"neochat/ratelimit"
	"neochat/router"
	"neochat/tokenizer"
)

func testCatalog() router.Catalog {
	return router.Catalog{Models: []router.Model{
		{
			ID: "test-instant", Provider: "openai", Modes: []string{"instant"},
			CostInputPerMTok: 1, CostOutputPerMTok: 2, ContextWindow: 50_000,
			SupportsModality: []string{"text", "code"},
			MaxOutputTokens:  4096, SupportsOutputFormats: []string{"text", "markdown"},
		},
		{
			ID: "test-thinking", Provider: "openai", Modes: []string{"instant", "thinking"},
			CostInputPerMTok: 3, CostOutputPerMTok: 10, ContextWindow: 200_000,
			SupportsModality: []string{"text", "code"},
			MaxOutputTokens:  16384, SupportsOutputFormats: []string{"text", "markdown"},
		},
	}}
}

func testWeights() router.Weights {
	return router.Weights{
		ConfidenceEscalationThreshold: 0.5,
		AutoMode: router.AutoModeWeights{
			ReasoningWeight: 1.0, ComplexityWeight: 2.0, CreativityWeight: 0.2,
			InstantCeiling: 1.0, ThinkingCeiling: 2.5,
		},
		TaskProfile: router.TaskProfileWeights{
			CategoryWeight: 0.8, IntentWeight: 0.2, DefaultScore: 0.5,
			MinimumScore: 0.65, MaxQualityGap: 0.05,
		},
	}
}

func newTestServer(t *testing.T, genResponses []provider.GenerateResult) (*Server, *provider.FakeClient) {
	t.Helper()

	genClient := &provider.FakeClient{Responses: genResponses}

	return &Server{
		Router: router.NewRouter(testCatalog(), testWeights()),

		Conversations: conversation.NewInMemoryStore(),
		Store:         limits.NewInMemorySpendStore(),
		Idempotency:   idempotency.NewInMemoryStore(),
		CostLog:       costlog.NewInMemoryStore(),
		Plans: map[string]limits.PlanLimits{
			"pro": {PlanID: "pro", ThinkingMaxCapUSD: 10.0, InstantExtraCapUSD: 3.0},
		},
		Generators: map[string]provider.Client{"openai": genClient},
		Auth:       auth.NewInMemoryStore(),
	}, genClient
}

// authHeader mints a fresh API key for (userID, planID) against s.Auth --
// an *auth.InMemoryStore for every Server newTestServer builds -- and
// returns the "Authorization" header value a test request needs to reach
// handleChat/handleChatStream as that identity. Tests that call
// s.handle/s.handleStream directly with a chatRequest{UserID: ...}
// literal never go through authenticated at all, so they don't need this;
// only the TestServeHTTP_* tests exercising the real net/http path do.
func authHeader(t *testing.T, s *Server, userID, planID string) string {
	t.Helper()
	store, ok := s.Auth.(*auth.InMemoryStore)
	if !ok {
		t.Fatalf("authHeader: s.Auth is %T, want *auth.InMemoryStore", s.Auth)
	}
	token, err := store.IssueKey(context.Background(), userID, planID)
	if err != nil {
		t.Fatalf("authHeader: IssueKey: %v", err)
	}
	return "Bearer " + token
}

func TestHandle_HappyPath(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000}
	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.SelectedModelID != "test-thinking" {
		t.Errorf("SelectedModelID = %q, want test-thinking", resp.SelectedModelID)
	}
	if resp.ResponseText != "here is the answer" {
		t.Errorf("ResponseText = %q", resp.ResponseText)
	}
	wantCost := router.ComputeCostUSD(router.Model{CostInputPerMTok: 3, CostOutputPerMTok: 10}, 1000, 500)
	if resp.ActualCostUSD != wantCost {
		t.Errorf("ActualCostUSD = %.6f, want %.6f", resp.ActualCostUSD, wantCost)
	}

	spent, err := s.Store.Sum(context.Background(), "u1", limits.PoolThinkingMax, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(spent-wantCost) > 1e-12 {
		t.Errorf("recorded thinking_max spend = %.6f, want %.6f", spent, wantCost)
	}

	if len(genClient.Requests) != 1 || genClient.Requests[0].APIModelID != "test-thinking" {
		t.Errorf("expected exactly 1 generate call against test-thinking, got %+v", genClient.Requests)
	}
	if resp.ConversationID == "" {
		t.Error("expected a generated ConversationID when the request didn't supply one")
	}
}

func TestHandle_RecordsCostLogEntry(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000}
	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := s.CostLog.(*costlog.InMemoryStore).Entries()
	got, ok := findCostLogEntry(entries, "thinking")
	if !ok {
		t.Fatalf("no cost_log entry with Mode=thinking among %+v", entries)
	}
	if got.UserID != "u1" {
		t.Errorf("UserID = %q, want u1", got.UserID)
	}
	if got.ModelID != "test-thinking" {
		t.Errorf("ModelID = %q, want test-thinking", got.ModelID)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 500 {
		t.Errorf("tokens = %d/%d, want 1000/500", got.InputTokens, got.OutputTokens)
	}
	if got.CostUSD != resp.ActualCostUSD {
		t.Errorf("CostUSD = %.6f, want it to match the response's ActualCostUSD %.6f", got.CostUSD, resp.ActualCostUSD)
	}
	if got.RequestID == "" {
		t.Error("expected a non-empty RequestID")
	}
}

func findCostLogEntry(entries []router.CostLogEntry, mode string) (router.CostLogEntry, bool) {
	for _, e := range entries {
		if e.Mode == mode {
			return e, true
		}
	}
	return router.CostLogEntry{}, false
}

// TestHandle_PrependsSystemPromptWhenConfigured checks that a configured
// Server.SystemPrompt is sent to the generation model as the first
// message, ahead of conversation history and the new user message, for the
// request's chosen persona -- defaulting to "Default" when the request
// doesn't specify one.
func TestHandle_PrependsSystemPromptForSelectedPersona(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})
	s.SystemPrompts = map[string]string{
		"Default": "you are a helpful assistant",
		"Expert":  "you are a domain expert, terse and precise",
	}

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000, Persona: "Expert"}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(genClient.Requests) != 1 {
		t.Fatalf("expected exactly 1 generate call, got %d", len(genClient.Requests))
	}
	msgs := genClient.Requests[0].Messages
	if len(msgs) == 0 || msgs[0].Role != "system" || msgs[0].Content != s.SystemPrompts["Expert"] {
		t.Fatalf("expected first message to be the Expert system prompt, got %+v", msgs)
	}
	if last := msgs[len(msgs)-1]; last.Role != "user" || last.Content != req.Message {
		t.Errorf("expected last message to be the user's request, got %+v", last)
	}
}

// TestHandle_DefaultsToDefaultPersonaWhenUnspecified checks an empty
// Persona field resolves to "Default", not "no persona at all".
func TestHandle_DefaultsToDefaultPersonaWhenUnspecified(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})
	s.SystemPrompts = map[string]string{"Default": "you are a helpful assistant"}

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := genClient.Requests[0].Messages
	if len(msgs) == 0 || msgs[0].Role != "system" || msgs[0].Content != "you are a helpful assistant" {
		t.Fatalf("expected the Default persona's prompt, got %+v", msgs)
	}
}

// TestHandle_NoSystemMessageWhenPersonaPromptNotWrittenYet checks that a
// valid persona name (one of SystemPromptNames) with no entry in
// Server.SystemPrompts (its prompt file is still an empty placeholder --
// see LoadSystemPrompts) sends no system message at all, rather than an
// empty one.
func TestHandle_NoSystemMessageWhenPersonaPromptNotWrittenYet(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000, Persona: "Cynical"}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(genClient.Requests) != 1 {
		t.Fatalf("expected exactly 1 generate call, got %d", len(genClient.Requests))
	}
	for _, m := range withoutIdentityNote(genClient.Requests[0].Messages) {
		if m.Role == "system" {
			t.Errorf("expected no system message for a persona with no loaded prompt, got %+v", genClient.Requests[0].Messages)
		}
	}
}

func TestServeHTTP_UnknownPersonaRejected(t *testing.T) {
	s, genClient := newTestServer(t, nil)

	body, _ := json.Marshal(chatRequest{Message: "hi", Persona: "Snarky"})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if len(genClient.Requests) != 0 {
		t.Errorf("expected no generate calls for a rejected request, got %d", len(genClient.Requests))
	}
}

// TestHandle_GenerateRespectsContextCancellation is the same check at the
// generation stage: a client disconnecting mid-generation must not leave
// the upstream call (and its cost) running to completion unobserved.
func TestHandle_GenerateRespectsContextCancellation(t *testing.T) {
	s, _ := newTestServer(t, nil)
	blockedGen := &provider.FakeClient{Block: make(chan struct{})}
	s.Generators = map[string]provider.Client{"openai": blockedGen}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 100}
	start := time.Now()
	_, err := s.handle(ctx, req, s.Plans["pro"])
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error once the context deadline is exceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the error to wrap context.DeadlineExceeded, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("handle() took %s to return, want well under 2s", elapsed)
	}
	if len(blockedGen.Requests) != 1 {
		t.Errorf("expected exactly 1 generate call to have started, got %d", len(blockedGen.Requests))
	}
}

// TestHandle_ConversationHistoryCarriesToNextTurn checks the actual point
// of conversation/: a second request on the same conversation_id sends
// the model both the first turn's user message and its own prior answer,
// not just the new message in isolation.
func TestHandle_ConversationHistoryCarriesToNextTurn(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "first answer", InputTokens: 100, OutputTokens: 50},
		{Text: "second answer", InputTokens: 100, OutputTokens: 50},
	})
	// scripted replies each, not newTestServer's default one.

	req1 := chatRequest{UserID: "u1", PlanID: "pro", Message: "what is recursion?", RequestedMode: "instant", EstimatedContextTokens: 100}
	resp1, err := s.handle(context.Background(), req1, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error on turn 1: %v", err)
	}

	req2 := chatRequest{UserID: "u1", PlanID: "pro", ConversationID: resp1.ConversationID, Message: "give an example", RequestedMode: "instant", EstimatedContextTokens: 100}
	if _, err := s.handle(context.Background(), req2, s.Plans["pro"]); err != nil {
		t.Fatalf("unexpected error on turn 2: %v", err)
	}

	if len(genClient.Requests) != 2 {
		t.Fatalf("expected 2 generate calls, got %d", len(genClient.Requests))
	}
	turn2Messages := withoutIdentityNote(genClient.Requests[1].Messages)
	if len(turn2Messages) != 3 {
		t.Fatalf("turn 2 sent %d messages, want 3 (prior user + prior assistant + new user): %+v", len(turn2Messages), turn2Messages)
	}
	if turn2Messages[0].Role != "user" || turn2Messages[0].Content != "what is recursion?" {
		t.Errorf("turn2Messages[0] = %+v, want the first turn's user message", turn2Messages[0])
	}
	if turn2Messages[1].Role != "assistant" || turn2Messages[1].Content != "first answer" {
		t.Errorf("turn2Messages[1] = %+v, want the first turn's assistant reply", turn2Messages[1])
	}
	if turn2Messages[2].Role != "user" || turn2Messages[2].Content != "give an example" {
		t.Errorf("turn2Messages[2] = %+v, want the new user message", turn2Messages[2])
	}

	history, err := s.Conversations.History(context.Background(), "u1", resp1.ConversationID, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("len(History()) = %d, want 4 (2 turns x user+assistant)", len(history))
	}
	if history[3].ModelID != "test-instant" {
		t.Errorf("stored assistant message's ModelID = %q, want test-instant", history[3].ModelID)
	}
}

func TestHandle_IncognitoUsesRequestHistoryWithoutPersisting(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{{Text: "private answer", InputTokens: 100, OutputTokens: 50}})
	req := chatRequest{
		UserID: "u1", PlanID: "pro", Message: "follow up", RequestedMode: "instant", Incognito: true,
		IncognitoHistory: []incognitoMessage{{Role: "user", Content: "private question"}, {Role: "assistant", Content: "private reply"}},
	}
	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(genClient.Requests) != 1 {
		t.Fatalf("generate calls = %d, want 1", len(genClient.Requests))
	}
	want := []provider.Message{{Role: "user", Content: "private question"}, {Role: "assistant", Content: "private reply"}, {Role: "user", Content: "follow up"}}
	got := withoutIdentityNote(genClient.Requests[0].Messages)
	if len(got) != len(want) {
		t.Fatalf("generated messages = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index].Role != want[index].Role || got[index].Content != want[index].Content || len(got[index].Parts) != 0 {
			t.Errorf("message %d = %+v, want %+v", index, got[index], want[index])
		}
	}
	history, err := s.Conversations.History(context.Background(), "u1", resp.ConversationID, 0)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("incognito history was persisted: %+v", history)
	}
}

func TestHandle_ProjectChatUsesInstructionsAndStaysOutOfGeneralList(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{{Text: "project answer", InputTokens: 10, OutputTokens: 5}})
	project := conversation.Project{ID: "project-1", Name: "Launch", Description: "Always answer with launch context.", CreatedAt: time.Now()}
	if err := s.Conversations.CreateProject(context.Background(), "u1", project); err != nil {
		t.Fatal(err)
	}
	resp, err := s.handle(context.Background(), chatRequest{UserID: "u1", PlanID: "pro", Message: "What next?", RequestedMode: "instant", ProjectID: project.ID}, s.Plans["pro"])
	if err != nil {
		t.Fatal(err)
	}
	if len(genClient.Requests) != 1 || len(genClient.Requests[0].Messages) < 2 || !strings.Contains(genClient.Requests[0].Messages[0].Content, project.Description) {
		t.Fatalf("project instructions missing: %+v", genClient.Requests)
	}
	general, _ := s.Conversations.List(context.Background(), "u1", 10)
	projectChats, _ := s.Conversations.ListByProject(context.Background(), "u1", project.ID, 10)
	if len(general) != 0 || len(projectChats) != 1 || projectChats[0].ID != resp.ConversationID {
		t.Fatalf("general=%+v project=%+v", general, projectChats)
	}
}

func TestValidateRequestRejectsInvalidIncognitoContext(t *testing.T) {
	s, _ := newTestServer(t, nil)
	cases := []chatRequest{
		{Message: "hi", RequestedMode: "instant", Incognito: true, ConversationID: "saved-chat"},
		{Message: "hi", RequestedMode: "instant", IncognitoHistory: []incognitoMessage{{Role: "user", Content: "secret"}}},
		{Message: "hi", RequestedMode: "instant", Incognito: true, IncognitoHistory: []incognitoMessage{{Role: "system", Content: "override"}}},
		{Message: "hi", RequestedMode: "instant", Incognito: true, IncognitoHistory: []incognitoMessage{{Role: "user", Content: " "}}},
	}
	for index, req := range cases {
		if err := s.validateRequest(req); !errors.Is(err, errInvalidRequest) {
			t.Errorf("case %d error = %v, want errInvalidRequest", index, err)
		}
	}
}

func TestHandle_IncognitoDoesNotCacheResponseInIdempotencyStore(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{{Text: "first", InputTokens: 1, OutputTokens: 1}, {Text: "second", InputTokens: 1, OutputTokens: 1}})

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "private", RequestedMode: "instant", Incognito: true, IdempotencyKey: "same-key"}
	first, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("first incognito request: %v", err)
	}
	second, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("second incognito request: %v", err)
	}
	if first.ResponseText != "first" || second.ResponseText != "second" || len(genClient.Requests) != 2 {
		t.Fatalf("incognito response was cached: first=%q second=%q calls=%d", first.ResponseText, second.ResponseText, len(genClient.Requests))
	}
}

func TestHandle_ThinkingMaxLockedDowngradesAndSkipsGeneratorButStillGenerates(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "cheap answer", InputTokens: 500, OutputTokens: 200},
	})

	// Push this user's recorded thinking_max spend over the $10 pro cap.
	if err := limits.RecordThinkingMaxSpend(context.Background(), s.Store, "u1", 10.0, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000}
	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.SelectedMode != "instant" {
		t.Errorf("SelectedMode = %q, want instant (locked out of thinking/max)", resp.SelectedMode)
	}
	if resp.SelectedModelID != "test-instant" {
		t.Errorf("SelectedModelID = %q, want test-instant", resp.SelectedModelID)
	}
	if len(genClient.Requests) != 1 || genClient.Requests[0].APIModelID != "test-instant" {
		t.Errorf("expected exactly 1 generate call against test-instant, got %+v", genClient.Requests)
	}
}

func TestHandle_InstantCapExceededRejectsBeforeGeneration(t *testing.T) {
	s, genClient := newTestServer(t, nil)

	// Push this user's recorded instant spend over the $3 pro cap.
	if err := limits.RecordInstantSpend(context.Background(), s.Store, "u1", 3.0, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500}
	_, err := s.handle(context.Background(), req, s.Plans["pro"])
	if !errors.Is(err, ErrInstantCapExceeded) {
		t.Fatalf("err = %v, want ErrInstantCapExceeded", err)
	}

	if len(genClient.Requests) != 0 {
		t.Errorf("generator was called %d times, want 0", len(genClient.Requests))
	}
}

// TestServeHTTP_InstantCapExceededReturns429 checks the HTTP layer maps
// ErrInstantCapExceeded to 429, not the generic 502 every other handle
// error gets.
func TestServeHTTP_InstantCapExceededReturns429(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if err := limits.RecordInstantSpend(context.Background(), s.Store, "u1", 3.0, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
}

// TestServeHTTP_RequestBodyTooLarge checks decodeChatRequest's
// http.MaxBytesReader guard actually rejects an oversized body with 413,
// instead of json.Decode reading it into memory in full (audit.md finding
// #3).
func TestServeHTTP_RequestBodyTooLarge(t *testing.T) {
	s, _ := newTestServer(t, nil)

	oversized := chatRequest{
		Message: strings.Repeat("a", maxRequestBodyBytes+1),
	}
	body, _ := json.Marshal(oversized)
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

// TestServeHTTP_RateLimitedIPReturns429 checks Mux's rateLimited wrapping
// actually rejects a request once IPRateLimiter says no, before
// decodeChatRequest/handle ever run.
func TestServeHTTP_RateLimitedIPReturns429(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "hello", InputTokens: 100, OutputTokens: 50},
	})
	s.IPRateLimiter = ratelimit.NewInMemoryLimiter(1, time.Minute)

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	bearer := authHeader(t, s, "u1", "pro")

	req1 := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req1.Header.Set("Authorization", bearer)
	rec1 := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st request status = %d, want %d, body = %s", rec1.Code, http.StatusOK, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req2.Header.Set("Authorization", bearer)
	rec2 := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("2nd request status = %d, want %d, body = %s", rec2.Code, http.StatusTooManyRequests, rec2.Body.String())
	}

	if len(genClient.Requests) != 1 {
		t.Errorf("generator was called %d times, want 1 (2nd request must be rejected before reaching it)", len(genClient.Requests))
	}
}

// TestServeHTTP_RateLimitedUserReturns429 is userRateLimited's
// counterpart to the IP test above: once UserRateLimiter says no, the
// request is rejected before decodeChatRequest/handle run at all.
func TestServeHTTP_RateLimitedUserReturns429(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "hello", InputTokens: 100, OutputTokens: 50},
	})
	s.UserRateLimiter = ratelimit.NewInMemoryLimiter(1, time.Minute)

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	bearer := authHeader(t, s, "u1", "pro")

	req1 := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req1.Header.Set("Authorization", bearer)
	rec1 := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st request status = %d, want %d, body = %s", rec1.Code, http.StatusOK, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req2.Header.Set("Authorization", bearer)
	rec2 := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("2nd request status = %d, want %d, body = %s", rec2.Code, http.StatusTooManyRequests, rec2.Body.String())
	}

	if len(genClient.Requests) != 1 {
		t.Errorf("generator was called %d times, want 1 (2nd request must be rejected before reaching it)", len(genClient.Requests))
	}
}

// TestServeHTTP_UserRateLimitFollowsKeyAcrossIPs is the whole reason
// UserRateLimiter exists alongside IPRateLimiter (audit.md finding #2):
// one credential driving traffic from many source addresses. Each
// request here comes from a different IP, so a generous per-IP limiter
// never trips -- every address is on its first request -- and only a
// key-keyed counter can see that it's all one caller.
func TestServeHTTP_UserRateLimitFollowsKeyAcrossIPs(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "hello", InputTokens: 100, OutputTokens: 50},
	})
	s.IPRateLimiter = ratelimit.NewInMemoryLimiter(100, time.Minute)
	s.UserRateLimiter = ratelimit.NewInMemoryLimiter(1, time.Minute)

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	bearer := authHeader(t, s, "u1", "pro")

	send := func(remoteAddr string) int {
		req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
		req.Header.Set("Authorization", bearer)
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		s.Mux().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send("198.51.100.1:1111"); code != http.StatusOK {
		t.Fatalf("1st request status = %d, want %d", code, http.StatusOK)
	}
	if code := send("203.0.113.7:2222"); code != http.StatusTooManyRequests {
		t.Errorf("2nd request from a different IP status = %d, want %d -- the per-key limit must not reset per source address", code, http.StatusTooManyRequests)
	}

	if len(genClient.Requests) != 1 {
		t.Errorf("generator was called %d times, want 1", len(genClient.Requests))
	}
}

// TestServeHTTP_UserRateLimitIsPerUserNotGlobal checks the limiter is
// keyed on the identity rather than counting every authenticated request
// into one shared bucket -- one user exhausting their quota must not
// throttle everyone else.
func TestServeHTTP_UserRateLimitIsPerUserNotGlobal(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "a", InputTokens: 10, OutputTokens: 5},
		{Text: "b", InputTokens: 10, OutputTokens: 5},
	})
	// Two requests reach the pipeline here (u1's first and u2's), so the

	s.UserRateLimiter = ratelimit.NewInMemoryLimiter(1, time.Minute)

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})

	send := func(bearer string) int {
		req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
		req.Header.Set("Authorization", bearer)
		rec := httptest.NewRecorder()
		s.Mux().ServeHTTP(rec, req)
		return rec.Code
	}

	u1, u2 := authHeader(t, s, "u1", "pro"), authHeader(t, s, "u2", "pro")
	if code := send(u1); code != http.StatusOK {
		t.Fatalf("u1's 1st request status = %d, want %d", code, http.StatusOK)
	}
	if code := send(u1); code != http.StatusTooManyRequests {
		t.Fatalf("u1's 2nd request status = %d, want %d", code, http.StatusTooManyRequests)
	}
	if code := send(u2); code != http.StatusOK {
		t.Errorf("u2's 1st request status = %d, want %d -- u1 exhausting their quota must not throttle u2", code, http.StatusOK)
	}
}

// erroringLimiter stands in for a limiter whose backing store is
// unreachable (Redis down), the case both limiters deliberately fail
// open on.
type erroringLimiter struct{}

func (erroringLimiter) Allow(context.Context, string) (bool, error) {
	return false, errors.New("redis unreachable")
}

// TestServeHTTP_UserRateLimiterErrorFailsClosed pins down the deliberate
// choice in userRateLimited: a limiter that errors must not read as
// "over quota". Note the stub returns allowed=false alongside its error,
// so a naive implementation checking the bool before the error would
// reject here -- failing open has to be explicit, and this catches it if
// it stops being.
func TestServeHTTP_UserRateLimiterErrorFailsClosed(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "hello", InputTokens: 100, OutputTokens: 50},
	})
	s.UserRateLimiter = erroringLimiter{}

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d -- a limiter error must fail open, not reject the request", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestServeHTTP_UnauthenticatedRequestNeverReachesUserRateLimiter checks
// the middleware ordering holds: userRateLimited sits inside
// authenticated, so a request with no valid credential is rejected with
// 401 without consuming anyone's per-user quota. Were the order
// inverted, an attacker with no key could burn a victim's quota by
// naming them.
func TestServeHTTP_UnauthenticatedRequestNeverReachesUserRateLimiter(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "hello", InputTokens: 100, OutputTokens: 50},
	})
	s.UserRateLimiter = ratelimit.NewInMemoryLimiter(1, time.Minute)

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})

	unauth := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	unauthRec := httptest.NewRecorder()
	s.Mux().ServeHTTP(unauthRec, unauth)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request status = %d, want %d", unauthRec.Code, http.StatusUnauthorized)
	}

	// The one real request the quota allows must still be available.
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated request status = %d, want %d -- the rejected unauthenticated request must not have consumed quota", rec.Code, http.StatusOK)
	}
}

// TestHandle_CircuitBreakerOpenSurfacesAsGenerateError checks that once a
// model's circuit is open and no other candidate model survives Route's
// hard filters (a single-model catalog here), handle has nothing left to
// fail over to and surfaces the CircuitOpenError-driven routing failure
// instead of retrying forever.
func TestHandle_CircuitBreakerOpenSurfacesAsGenerateError(t *testing.T) {
	failingClient := &provider.FakeClient{Err: errors.New("upstream down")}
	breaker := provider.NewCircuitBreakerClient(failingClient, 1, time.Minute)

	singleModelCatalog := router.Catalog{Models: []router.Model{testCatalog().Models[0]}}

	s, _ := newTestServer(t, nil)
	s.Router = router.NewRouter(singleModelCatalog, testWeights())
	s.Generators = map[string]provider.Client{"openai": breaker}
	// each; this test calls handle() twice, so give both two.

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 100}

	// First call: reaches the underlying client, fails, trips the
	// (threshold-1) breaker.
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err == nil {
		t.Fatal("expected an error from the first call")
	}
	if len(failingClient.Requests) != 1 {
		t.Fatalf("expected exactly 1 call to reach the underlying client, got %d", len(failingClient.Requests))
	}

	// Second call: circuit is open, handle retries Route with the model
	// excluded, finds no other candidate (single-model catalog), and
	// returns that routing failure -- without ever touching the
	// underlying client again.
	_, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err == nil {
		t.Fatal("expected an error from the second call")
	}
	if len(failingClient.Requests) != 1 {
		t.Errorf("expected the second call to skip the underlying client, still got %d calls", len(failingClient.Requests))
	}
}

// TestHandle_CircuitOpenFailsOverToHealthyModel checks the actual failover
// path: once the cheapest candidate's circuit is open, handle re-routes
// around it within a single call and successfully generates from the
// next-cheapest surviving model instead of failing the whole request.
func TestHandle_CircuitOpenFailsOverToHealthyModel(t *testing.T) {
	failoverClient := &provider.FakeClient{
		Err: errors.New("upstream down"),
		PerModel: map[string]provider.GenerateResult{
			"test-thinking": {Text: "healthy reply", InputTokens: 10, OutputTokens: 5},
		},
	}
	breaker := provider.NewCircuitBreakerClient(failoverClient, 1, time.Minute)

	s, _ := newTestServer(t, nil)
	s.Generators = map[string]provider.Client{"openai": breaker}
	// each; this test calls handle() twice, so give both two.

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 100}

	// First call: test-instant (the cheapest instant-tier candidate) is
	// tried, fails with a real upstream error, and trips its
	// (threshold=1) circuit. No failover here -- the first failure isn't
	// a CircuitOpenError, so this call still fails.
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err == nil {
		t.Fatal("expected an error from the first call")
	}

	// Second call: test-instant's circuit is now open, so handle gets a
	// CircuitOpenError on the first attempt, excludes test-instant, and
	// retries Route -- which picks test-thinking, the only other
	// instant-tier candidate. That model's circuit is untripped and its
	// FakeClient response is scripted to succeed.
	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("expected failover to the healthy model to succeed, got error: %v", err)
	}
	if resp.SelectedModelID != "test-thinking" {
		t.Errorf("expected failover to pick test-thinking, got %q", resp.SelectedModelID)
	}
	if resp.ResponseText != "healthy reply" {
		t.Errorf("expected the healthy model's reply, got %q", resp.ResponseText)
	}
	if len(failoverClient.Requests) != 2 {
		t.Errorf("expected 2 calls to the underlying client (failed test-instant, succeeded test-thinking), got %d", len(failoverClient.Requests))
	}
}

// fakeFailingBlockLog always errors on Record -- used to check that a
// logging failure doesn't undo the block itself.

// TestServeHTTP_PanicInHandlerReturns500 covers the HTTP-level half of the
// same fix: recovered() turns a handler panic into a logged 500 with a
// generic body, rather than net/http dropping the connection with no
// response at all.
func TestServeHTTP_PanicInHandlerReturns500(t *testing.T) {
	h := recovered(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := rec.Body.String(); strings.Contains(got, "handler exploded") {
		t.Errorf("response body leaked the panic value: %q", got)
	}
}

// abortingStreamClient streams a few deltas and then fails, standing in
// for the case that matters: a generation the vendor really did run (and
// bill for) whose result never reached the user -- a client that closed
// the tab mid-answer, or an upstream that dropped the connection.
type abortingStreamClient struct {
	deltas []string
}

func (c *abortingStreamClient) Generate(context.Context, string, []provider.Message) (provider.GenerateResult, error) {
	return provider.GenerateResult{}, errors.New("upstream died")
}

func (c *abortingStreamClient) GenerateStream(_ context.Context, _ string, _ []provider.Message) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk)
	go func() {
		defer close(ch)
		for _, d := range c.deltas {
			ch <- provider.StreamChunk{Delta: d}
		}
		ch <- provider.StreamChunk{Err: errors.New("upstream died mid-stream")}
	}()
	return ch, nil
}

// TestHandleStream_AbortedGenerationIsStillBilled is the regression test
// for audit.md's aborted-spend finding. finalize is what records spend,
// and it only runs on success -- so before this, a generation that
// streamed most of an answer and then died left no cost_log row and no
// movement in the rolling spend window, while the vendor had very much
// billed for the tokens. Repeatable on purpose by a client that
// disconnects just before completion.
func TestHandleStream_AbortedGenerationIsStillBilled(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.Generators = map[string]provider.Client{
		"openai": &abortingStreamClient{deltas: []string{"here is ", "most of the ", "answer "}},
	}

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500}
	err := s.handleStream(context.Background(), req, s.Plans["pro"], func(string, any) {})
	if err == nil {
		t.Fatal("expected the aborted stream to surface as an error")
	}

	entries := s.CostLog.(*costlog.InMemoryStore).Entries()
	if len(entries) != 1 {
		t.Fatalf("cost_log has %d entries, want only the aborted generation", len(entries))
	}
	var aborted *router.CostLogEntry
	for i := range entries {
		if strings.HasSuffix(entries[i].Mode, "_aborted") {
			aborted = &entries[i]
		}
	}
	if aborted == nil {
		t.Fatalf("no _aborted cost_log entry recorded, got modes: %v", entries)
	}
	if aborted.OutputTokens <= 0 {
		t.Errorf("aborted entry OutputTokens = %d, want > 0 (estimated from the text that actually streamed)", aborted.OutputTokens)
	}
	if aborted.CostUSD <= 0 {
		t.Errorf("aborted entry CostUSD = %f, want > 0", aborted.CostUSD)
	}

	// The rolling spend window has to move too, or the cap can be bypassed
	// indefinitely by disconnecting every time.
	spent, err := s.Store.Sum(context.Background(), "u1", limits.PoolInstant, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spent <= 0 {
		t.Errorf("instant spend = %f, want > 0 -- an aborted generation must still count against the cap", spent)
	}
}

// TestHandleStream_AbortedGenerationBilledAfterClientDisconnect is the
// same guarantee under the condition that actually triggers it in
// production: the request context is already cancelled. The accounting
// writes must not be made against that dead context, or every one of them
// fails and the hole is right back.
func TestHandleStream_AbortedGenerationBilledAfterClientDisconnect(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.Generators = map[string]provider.Client{
		"openai": &abortingStreamClient{deltas: []string{"partial answer text "}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500}

	// Cancel as soon as the first delta reaches the client, mimicking a
	// browser tab closing mid-answer.
	err := s.handleStream(ctx, req, s.Plans["pro"], func(event string, _ any) {
		if event == "delta" {
			cancel()
		}
	})
	defer cancel()
	if err == nil {
		t.Fatal("expected an error once the stream aborted")
	}

	spent, sumErr := s.Store.Sum(context.Background(), "u1", limits.PoolInstant, 30*24*time.Hour)
	if sumErr != nil {
		t.Fatalf("unexpected error: %v", sumErr)
	}
	if spent <= 0 {
		t.Errorf("instant spend = %f, want > 0 -- billing must survive the request context being cancelled", spent)
	}
}

// TestClientErrorMessage pins down clientErrorMessage's allowlist
// directly: the two sentinel errors it's meant to expose verbatim, and
// an arbitrary internal error it must not.
func TestClientErrorMessage(t *testing.T) {
	if got := clientErrorMessage(ErrInstantCapExceeded); got != ErrInstantCapExceeded.Error() {
		t.Errorf("clientErrorMessage(ErrInstantCapExceeded) = %q, want the sentinel's own message", got)
	}

	wrapped := fmt.Errorf("a request with idempotency_key %q is already in progress for this user: %w", "k1", idempotency.ErrInFlight)
	if got := clientErrorMessage(wrapped); got != wrapped.Error() {
		t.Errorf("clientErrorMessage(wrapped ErrInFlight) = %q, want %q", got, wrapped.Error())
	}

	internal := fmt.Errorf("generate: provider: openrouter error (status 401): invalid api key")
	if got := clientErrorMessage(internal); got == internal.Error() {
		t.Errorf("clientErrorMessage(unrecognized internal error) returned the raw error text verbatim: %q", got)
	}
}

func TestServeHTTP_InternalErrorDoesNotLeakDetails(t *testing.T) {
	s, gen := newTestServer(t, nil)
	gen.Err = errors.New("internal upstream failure")

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := rec.Body.String(); strings.Contains(got, "internal upstream failure") {
		t.Errorf("response body leaked the internal error text: %q", got)
	}
}

// TestServeHTTP_InstantCapExceededMessageNotGeneric checks the one
// allowlisted case: ErrInstantCapExceeded's own message (safe,
// user-actionable) still reaches the caller verbatim instead of being
// replaced by clientErrorMessage's generic fallback.
func TestServeHTTP_InstantCapExceededMessageNotGeneric(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if err := limits.RecordInstantSpend(context.Background(), s.Store, "u1", 3.0, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if got := rec.Body.String(); !strings.Contains(got, "instant-tier spend cap exceeded") {
		t.Errorf("response body = %q, want it to contain ErrInstantCapExceeded's own message", got)
	}
}

// TestServeHTTP_UnknownPlanID checks the plan_id lookup in
// decodeChatRequest for a plan_id that isn't a client mistake: plan_id
// now comes from the authenticated identity (whatever cmd/issuekey wrote
// into api_keys at issuance), not from the request body, so an unknown
// value here means that key's stored plan was since removed from
// configs/plans.json -- a server-side data-integrity problem, hence 500,
// not the 400 a bad client input would get.
func TestServeHTTP_UnknownPlanID(t *testing.T) {
	s, _ := newTestServer(t, nil)

	body, _ := json.Marshal(chatRequest{Message: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "bogus"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

// TestServeHTTP_HappyPath exercises the full JSON-in/JSON-out HTTP path,
// not just the internal handle() call the other tests use directly.
func TestServeHTTP_HappyPath(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "hello", InputTokens: 100, OutputTokens: 50},
	})

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ResponseText != "hello" {
		t.Errorf("ResponseText = %q, want hello", resp.ResponseText)
	}
}

// streamEvent captures one handleStream send(...) call for assertions.
type streamEvent struct {
	event   string
	payload any
}

// TestHandleStream_HappyPath checks the streaming pipeline emits a "meta"
// event once routing picks a model, one "delta" event per chunk the
// (fake) vendor streams, and a final "done" event carrying the same
// chatResponse handle() would have returned in the non-streaming path.
func TestHandleStream_HappyPath(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "hello there", InputTokens: 100, OutputTokens: 50},
	})

	var events []streamEvent
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500}
	err := s.handleStream(context.Background(), req, s.Plans["pro"], func(event string, payload any) {
		events = append(events, streamEvent{event, payload})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) < 2 {
		t.Fatalf("expected at least a meta and a done event, got %+v", events)
	}
	if events[0].event != "meta" {
		t.Errorf("first event = %q, want meta", events[0].event)
	}

	var deltaText string
	var sawDone bool
	var done chatResponse
	for _, ev := range events[1:] {
		switch ev.event {
		case "delta":
			deltaText += ev.payload.(map[string]string)["text"]
		case "done":
			sawDone = true
			done = ev.payload.(chatResponse)
		default:
			t.Errorf("unexpected event type %q", ev.event)
		}
	}
	if !sawDone {
		t.Fatal("expected a done event")
	}
	if deltaText != "hello there" {
		t.Errorf("concatenated deltas = %q, want %q", deltaText, "hello there")
	}
	if done.ResponseText != "hello there" {
		t.Errorf("done.ResponseText = %q, want %q", done.ResponseText, "hello there")
	}
	if len(genClient.Requests) != 1 {
		t.Errorf("expected exactly 1 generate call, got %d", len(genClient.Requests))
	}
}

// TestHandleStream_CircuitOpenFailsOverToHealthyModel mirrors
// TestHandle_CircuitOpenFailsOverToHealthyModel for the streaming path:
// once the cheapest model's circuit is open, handleStream re-routes to
// the next candidate and streams its reply instead of failing, emitting a
// second "meta" event for the model that actually answers.
func TestHandleStream_CircuitOpenFailsOverToHealthyModel(t *testing.T) {
	failoverClient := &provider.FakeClient{
		Err: errors.New("upstream down"),
		PerModel: map[string]provider.GenerateResult{
			"test-thinking": {Text: "healthy reply", InputTokens: 10, OutputTokens: 5},
		},
	}
	breaker := provider.NewCircuitBreakerClient(failoverClient, 1, time.Minute)

	s, _ := newTestServer(t, nil)
	s.Generators = map[string]provider.Client{"openai": breaker}

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 100}

	// First call trips test-instant's circuit (threshold=1), same as the
	// non-streaming version of this test.
	_ = s.handleStream(context.Background(), req, s.Plans["pro"], func(string, any) {})

	var events []streamEvent
	err := s.handleStream(context.Background(), req, s.Plans["pro"], func(event string, payload any) {
		events = append(events, streamEvent{event, payload})
	})
	if err != nil {
		t.Fatalf("expected failover to the healthy model to succeed, got error: %v", err)
	}

	var metaModelIDs []string
	var deltaText string
	for _, ev := range events {
		switch ev.event {
		case "meta":
			metaModelIDs = append(metaModelIDs, ev.payload.(map[string]any)["selected_model_id"].(string))
		case "delta":
			deltaText += ev.payload.(map[string]string)["text"]
		}
	}
	if len(metaModelIDs) != 2 || metaModelIDs[0] != "test-instant" || metaModelIDs[1] != "test-thinking" {
		t.Errorf("expected meta events for [test-instant, test-thinking], got %+v", metaModelIDs)
	}
	if deltaText != "healthy reply" {
		t.Errorf("streamed text = %q, want %q", deltaText, "healthy reply")
	}
}

// TestServeHTTP_ChatStream exercises the actual HTTP/SSE wire format for
// POST /chat/stream, not just handleStream's internal send() calls --
// checks the response headers and that the body contains properly framed
// "event: ...\ndata: ...\n\n" blocks ending in a "done" event.
func TestServeHTTP_ChatStream(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "hi back", InputTokens: 10, OutputTokens: 5},
	})

	body, _ := json.Marshal(chatRequest{Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: meta\n") {
		t.Errorf("expected a meta event in the SSE stream, got:\n%s", out)
	}
	if !strings.Contains(out, "event: done\n") {
		t.Errorf("expected a done event in the SSE stream, got:\n%s", out)
	}
	if !strings.Contains(out, `"response_text":"hi back"`) {
		t.Errorf("expected the done event to carry the generated text, got:\n%s", out)
	}
}

func TestServeHTTP_RegenerateStreamStoresVersionWithoutDuplicatingPrompt(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "original answer", InputTokens: 10, OutputTokens: 5},
		{Text: "alternate answer", InputTokens: 10, OutputTokens: 5},
	})

	initial, err := s.handle(context.Background(), chatRequest{UserID: "u1", PlanID: "pro", Message: "question", RequestedMode: "instant"}, s.Plans["pro"])
	if err != nil {
		t.Fatalf("initial response: %v", err)
	}

	history, err := s.Conversations.History(context.Background(), "u1", initial.ConversationID, 0)
	if err != nil || len(history) != 2 {
		t.Fatalf("initial history = %+v, err=%v", history, err)
	}
	body, _ := json.Marshal(regenerateRequest{ConversationID: initial.ConversationID, MessageID: history[1].ID, RequestedMode: "instant"})
	req := httptest.NewRequest(http.MethodPost, "/chat/regenerate/stream", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(t, s, "u1", "pro"))
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: versions\n") || !strings.Contains(out, `"content":"original answer"`) || !strings.Contains(out, `"content":"alternate answer"`) {
		t.Fatalf("version event missing saved alternatives: %s", out)
	}
	history, err = s.Conversations.History(context.Background(), "u1", initial.ConversationID, 0)
	if err != nil || len(history) != 2 {
		t.Fatalf("regenerated history = %+v, err=%v", history, err)
	}
	if history[0].Content != "question" || history[1].Content != "alternate answer" || len(history[1].Versions) != 2 {
		t.Fatalf("regenerated history = %+v", history)
	}
}

// TestHandle_IdempotencyKeyReplaysWithoutRegenerating checks the core
// duplicate-billing guard: retrying the exact same (user_id,
// idempotency_key) must not call the generation model a second time or
// record spend twice -- it should just replay the first attempt's response.
// Only one GenerateResult is scripted, so a second real generate call
// would panic FakeClient (see provider.FakeClient.Generate) -- the test
// passing at all is itself proof the second attempt never re-generated.
func TestHandle_IdempotencyKeyReplaysWithoutRegenerating(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})

	req := chatRequest{
		UserID: "u1", PlanID: "pro", Message: "explain recursion",
		RequestedMode: "thinking", EstimatedContextTokens: 2000,
		IdempotencyKey: "retry-key-1",
	}

	first, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("first attempt: unexpected error: %v", err)
	}

	second, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("retried attempt: unexpected error: %v", err)
	}

	if !reflect.DeepEqual(second, first) {
		t.Errorf("retried response = %+v, want identical to first attempt %+v", second, first)
	}
	if len(genClient.Requests) != 1 {
		t.Errorf("expected exactly 1 generate call across both attempts, got %d", len(genClient.Requests))
	}

	spent, err := s.Store.Sum(context.Background(), "u1", limits.PoolThinkingMax, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCost := router.ComputeCostUSD(router.Model{CostInputPerMTok: 3, CostOutputPerMTok: 10}, 1000, 500)
	if math.Abs(spent-wantCost) > 1e-12 {
		t.Errorf("recorded thinking_max spend = %.6f, want %.6f (should be billed once, not twice)", spent, wantCost)
	}
}

// TestHandle_IdempotencyKeyScopedPerUser checks that the same idempotency
// key from two different users is not treated as a collision -- each user
// gets their own generate call and their own billing, only a retry from the
// *same* user should be deduped.
func TestHandle_IdempotencyKeyScopedPerUser(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "answer for u1", InputTokens: 100, OutputTokens: 50},
		{Text: "answer for u2", InputTokens: 100, OutputTokens: 50},
	})
	// scripted replies each, not newTestServer's default one.

	base := chatRequest{PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500, IdempotencyKey: "same-key"}
	req1 := base
	req1.UserID = "u1"
	req2 := base
	req2.UserID = "u2"

	if _, err := s.handle(context.Background(), req1, s.Plans["pro"]); err != nil {
		t.Fatalf("u1: unexpected error: %v", err)
	}
	if _, err := s.handle(context.Background(), req2, s.Plans["pro"]); err != nil {
		t.Fatalf("u2: unexpected error: %v", err)
	}

	if len(genClient.Requests) != 2 {
		t.Errorf("expected 2 generate calls (one per user), got %d", len(genClient.Requests))
	}
}

// TestHandle_IdempotencyKeyReleasedAfterFailure checks that a failed
// attempt does not permanently poison its idempotency key: a legitimate
// retry after a real failure (as opposed to a retry after a dropped
// connection following success) must still be allowed to actually run.
func TestHandle_IdempotencyKeyReleasedAfterFailure(t *testing.T) {
	s, genClient := newTestServer(t, nil)
	genClient.Err = errors.New("upstream boom")
	// scripted replies each, not newTestServer's default one.

	req := chatRequest{
		UserID: "u1", PlanID: "pro", Message: "hi",
		RequestedMode: "instant", EstimatedContextTokens: 500,
		IdempotencyKey: "retry-key-2",
	}

	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err == nil {
		t.Fatal("expected the first attempt to fail")
	}

	genClient.Err = nil
	genClient.Responses = []provider.GenerateResult{{Text: "recovered", InputTokens: 10, OutputTokens: 5}}

	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("expected the retry to succeed once the upstream recovered, got: %v", err)
	}
	if resp.ResponseText != "recovered" {
		t.Errorf("ResponseText = %q, want %q", resp.ResponseText, "recovered")
	}
}

// TestHandle_NoIdempotencyKeyAlwaysRegenerates checks the guard is opt-in:
// a request that never sets IdempotencyKey gets no dedup at all, matching
// behavior from before this field existed.
func TestHandle_NoIdempotencyKeyAlwaysRegenerates(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "first", InputTokens: 10, OutputTokens: 5},
		{Text: "second", InputTokens: 10, OutputTokens: 5},
	})
	// scripted replies each, not newTestServer's default one.

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500}

	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}

	if len(genClient.Requests) != 2 {
		t.Errorf("expected 2 generate calls (no idempotency key means no dedup), got %d", len(genClient.Requests))
	}
}

// TestHandleStream_IdempotencyKeyReplaysWithoutRegenerating is
// TestHandle_IdempotencyKeyReplaysWithoutRegenerating's streaming
// counterpart: a retried request with the same idempotency key must replay
// the cached "done" event directly, with no "meta"/"delta" events (nothing
// is actually generated on a replay) and no second generate call.
func TestHandleStream_IdempotencyKeyReplaysWithoutRegenerating(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "hello there", InputTokens: 100, OutputTokens: 50},
	})

	req := chatRequest{
		UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500,
		IdempotencyKey: "stream-retry-key",
	}

	collect := func() []streamEvent {
		var events []streamEvent
		err := s.handleStream(context.Background(), req, s.Plans["pro"], func(event string, payload any) {
			events = append(events, streamEvent{event, payload})
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return events
	}

	first := collect()
	second := collect()

	if len(genClient.Requests) != 1 {
		t.Errorf("expected exactly 1 generate call across both attempts, got %d", len(genClient.Requests))
	}
	if len(second) != 1 || second[0].event != "done" {
		t.Errorf("replayed events = %+v, want exactly one done event", second)
	}

	var firstDone chatResponse
	for _, ev := range first {
		if ev.event == "done" {
			firstDone = ev.payload.(chatResponse)
		}
	}
	if !reflect.DeepEqual(second[0].payload.(chatResponse), firstDone) {
		t.Errorf("replayed done payload = %+v, want identical to first attempt's %+v", second[0].payload, firstDone)
	}
}

// TestPrepare_EstimatedContextTokensComputedFromRealMessages checks the
// actual fix: prepare no longer trusts chatRequest.EstimatedContextTokens
// -- it recomputes the real figure via tokenizer.EstimateMessages against
// the system prompt + stored history + new message that's actually about
// to be sent, so it grows as a conversation grows instead of staying
// pinned to whatever number (if any) the client declared.
func TestPrepare_EstimatedContextTokensComputedFromRealMessages(t *testing.T) {
	s, _ := newTestServer(t, nil)
	ctx := context.Background()

	if err := s.Conversations.Append(ctx, "u1", "conv1", conversation.Message{Role: conversation.RoleUser, Content: strings.Repeat("hello world ", 50), CreatedAt: time.Now()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.Conversations.Append(ctx, "u1", "conv1", conversation.Message{Role: conversation.RoleAssistant, Content: strings.Repeat("sure thing ", 50), CreatedAt: time.Now()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req := chatRequest{UserID: "u1", PlanID: "pro", ConversationID: "conv1", Message: "ok", RequestedMode: "instant", EstimatedContextTokens: 1}
	prepared, err := s.prepare(ctx, req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if prepared.estimatedContextTokens <= req.EstimatedContextTokens {
		t.Errorf("estimatedContextTokens = %d, want it computed from real history/message content, not stuck at the client-declared %d", prepared.estimatedContextTokens, req.EstimatedContextTokens)
	}
	if want := tokenizer.EstimateMessages(prepared.messages); prepared.estimatedContextTokens != want {
		t.Errorf("estimatedContextTokens = %d, want tokenizer.EstimateMessages(messages) = %d", prepared.estimatedContextTokens, want)
	}
}

// TestHandle_ContextWindowFilterUsesRealMessageSizeNotClientValue checks
// that a client can't dodge the router's context-window hard filter (or
// understate cost for abuse purposes) by simply declaring a small
// EstimatedContextTokens -- routing must fail here because the real
// message content exceeds the only candidate model's context window,
// regardless of what the client claimed.
func TestHandle_ContextWindowFilterUsesRealMessageSizeNotClientValue(t *testing.T) {
	catalog := router.Catalog{Models: []router.Model{
		{
			ID: "tiny-context", Provider: "openai", Modes: []string{"instant"},
			CostInputPerMTok: 1, CostOutputPerMTok: 2, ContextWindow: 100,
			SupportsModality: []string{"text", "code"},
			MaxOutputTokens:  4096, SupportsOutputFormats: []string{"text", "markdown"},
		},
	}}

	s := &Server{
		Router: router.NewRouter(catalog, testWeights()),

		Conversations: conversation.NewInMemoryStore(),
		Store:         limits.NewInMemorySpendStore(),
		Idempotency:   idempotency.NewInMemoryStore(),
		CostLog:       costlog.NewInMemoryStore(),
		Plans: map[string]limits.PlanLimits{
			"pro": {PlanID: "pro", ThinkingMaxCapUSD: 10.0, InstantExtraCapUSD: 3.0},
		},
		Generators: map[string]provider.Client{"openai": &provider.FakeClient{}},
	}

	req := chatRequest{
		UserID: "u1", PlanID: "pro", RequestedMode: "instant",
		Message:                strings.Repeat("word ", 1000), // real content far exceeds ContextWindow: 100
		EstimatedContextTokens: 1,                             // client lies small -- must not save it from the filter
	}

	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err == nil {
		t.Fatal("expected routing to fail: real message content exceeds the only model's context window, regardless of the client-declared EstimatedContextTokens")
	}
}

// A normal turn has one generation charge; there are no preflight AI calls.
func TestChatHasOnlyGenerationUsage(t *testing.T) {
	for _, mode := range []string{"manual", "instant", "thinking", "auto"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				s, gen := newTestServer(t, []provider.GenerateResult{{Text: "answer", InputTokens: 10, OutputTokens: 5}})
				req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: mode, ManualModelID: "test-instant"}
				var err error
				if stream {
					err = s.handleStream(context.Background(), req, s.Plans["pro"], func(string, any) {})
				} else {
					_, err = s.handle(context.Background(), req, s.Plans["pro"])
				}
				if err != nil {
					t.Fatal(err)
				}
				entries := s.CostLog.(*costlog.InMemoryStore).Entries()
				if len(gen.Requests) != 1 || len(entries) != 1 || entries[0].ModelID != gen.Requests[0].APIModelID {
					t.Fatalf("want only generation: calls=%d entries=%+v", len(gen.Requests), entries)
				}
			})
		}
	}
}
