package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"neochat/classifier"
	"neochat/conversation"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/router"
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
	}
}

const classifierReply = `{
	"schema_version": "1.0", "task_type": "qa", "language": "en",
	"modality_input": ["text"], "modality_output_expected": ["text"],
	"reasoning_depth": "high", "creativity_level": "low", "required_tools": [],
	"expected_output_length": "medium", "estimated_output_tokens": 300,
	"output_format": "", "complexity_score": 0.8, "context_dependency": "light",
	"confidence": 0.9, "content_flags": [], "safety_risk_score": 0.0
}`

const notFlaggedReply = `{"flagged": false, "categories": [], "reason": ""}`

func newTestServer(t *testing.T, genResponses []provider.GenerateResult) (*Server, *provider.FakeClient) {
	t.Helper()
	classifierClient := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}}}
	moderationClient := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}}}
	genClient := &provider.FakeClient{Responses: genResponses}

	return &Server{
		Router:        router.NewRouter(testCatalog(), testWeights()),
		Classifier:    classifier.New(classifierClient, "fake-classifier-model", "system prompt"),
		Moderator:     moderation.New(moderationClient, "fake-moderation-model", "system prompt"),
		ModerationLog: moderation.NewInMemoryBlockLog(),
		Conversations: conversation.NewInMemoryStore(),
		Store:         limits.NewInMemorySpendStore(),
		Plans: map[string]limits.PlanLimits{
			"pro": {PlanID: "pro", ThinkingMaxCapUSD: 10.0, InstantExtraCapUSD: 3.0},
		},
		Generators: map[string]provider.Client{"openai": genClient},
	}, genClient
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
	if spent != wantCost {
		t.Errorf("recorded thinking_max spend = %.6f, want %.6f", spent, wantCost)
	}

	if len(genClient.Requests) != 1 || genClient.Requests[0].APIModelID != "test-thinking" {
		t.Errorf("expected exactly 1 generate call against test-thinking, got %+v", genClient.Requests)
	}
	if resp.ConversationID == "" {
		t.Error("expected a generated ConversationID when the request didn't supply one")
	}
}

// TestHandle_PrependsSystemPromptWhenConfigured checks that a configured
// Server.SystemPrompt is sent to the generation model as the first
// message, ahead of conversation history and the new user message.
func TestHandle_PrependsSystemPromptWhenConfigured(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})
	s.SystemPrompt = "you are a helpful assistant"

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(genClient.Requests) != 1 {
		t.Fatalf("expected exactly 1 generate call, got %d", len(genClient.Requests))
	}
	msgs := genClient.Requests[0].Messages
	if len(msgs) == 0 || msgs[0].Role != "system" || msgs[0].Content != s.SystemPrompt {
		t.Fatalf("expected first message to be the system prompt, got %+v", msgs)
	}
	if last := msgs[len(msgs)-1]; last.Role != "user" || last.Content != req.Message {
		t.Errorf("expected last message to be the user's request, got %+v", last)
	}
}

// TestHandle_NoSystemMessageWhenPromptEmpty checks that leaving
// Server.SystemPrompt unset (its zero value, e.g. before the real prompt
// is written) sends no system message at all, rather than an empty one.
func TestHandle_NoSystemMessageWhenPromptEmpty(t *testing.T) {
	s, genClient := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(genClient.Requests) != 1 {
		t.Fatalf("expected exactly 1 generate call, got %d", len(genClient.Requests))
	}
	for _, m := range genClient.Requests[0].Messages {
		if m.Role == "system" {
			t.Errorf("expected no system message when SystemPrompt is unset, got %+v", genClient.Requests[0].Messages)
		}
	}
}

// TestHandle_ClassifyAndModerateRespectContextCancellation verifies the
// README pre-launch checklist's "request-level cancellation" item at the
// classify/moderate stage: if the client disconnects (net/http cancels
// r.Context()) while those two calls are still in flight, handle() must
// not hang waiting for them -- both goroutines should return promptly
// once ctx is done, since they're passed the same ctx as everything
// else.
func TestHandle_ClassifyAndModerateRespectContextCancellation(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.Classifier = classifier.New(&provider.FakeClient{Block: make(chan struct{})}, "fake-classifier-model", "system prompt")
	s.Moderator = moderation.New(&provider.FakeClient{Block: make(chan struct{})}, "fake-moderation-model", "system prompt")

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
	// Two handle() calls means the classifier/moderator fakes need two
	// scripted replies each, not newTestServer's default one.
	s.Classifier = classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}, {Text: classifierReply}}}, "fake-classifier-model", "system prompt")
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}, {Text: notFlaggedReply}}}, "fake-moderation-model", "system prompt")

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
	turn2Messages := genClient.Requests[1].Messages
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

	history, err := s.Conversations.History(context.Background(), "u1", resp1.ConversationID)
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

func TestHandle_ModerationFlaggedResponseStillCarriesConversationID(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{
		{Text: `{"flagged": true, "categories": ["illegal_activity"], "reason": "asks how to commit a crime"}`},
	}}, "fake-moderation-model", "system prompt")

	req := chatRequest{UserID: "u1", PlanID: "pro", ConversationID: "existing-thread", Message: "bad request", RequestedMode: "instant", EstimatedContextTokens: 100}
	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ConversationID != "existing-thread" {
		t.Errorf("ConversationID = %q, want existing-thread (unchanged by a block)", resp.ConversationID)
	}

	history, err := s.Conversations.History(context.Background(), "u1", "existing-thread")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected a blocked message not to be persisted to history, got %d entries", len(history))
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
	// newTestServer's Classifier/Moderator fakes only script one reply
	// each; this test calls handle() twice, so give both two.
	s.Classifier = classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}, {Text: classifierReply}}}, "fake-classifier-model", "system prompt")
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}, {Text: notFlaggedReply}}}, "fake-moderation-model", "system prompt")

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
	// newTestServer's Classifier/Moderator fakes only script one reply
	// each; this test calls handle() twice, so give both two.
	s.Classifier = classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}, {Text: classifierReply}}}, "fake-classifier-model", "system prompt")
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}, {Text: notFlaggedReply}}}, "fake-moderation-model", "system prompt")

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

func TestHandle_ModerationFlaggedBlocksBeforeGeneration(t *testing.T) {
	s, genClient := newTestServer(t, nil)
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{
		{Text: `{"flagged": true, "categories": ["illegal_activity"], "reason": "asks how to commit a crime"}`},
	}}, "fake-moderation-model", "system prompt")

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "bad request", RequestedMode: "instant", EstimatedContextTokens: 100}
	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Blocked {
		t.Errorf("Blocked = false, want true")
	}
	if resp.ResponseText != tosViolationMessage {
		t.Errorf("ResponseText = %q, want the ToS violation message", resp.ResponseText)
	}
	if resp.SelectedModelID != "" {
		t.Errorf("SelectedModelID = %q, want empty (no model was ever selected)", resp.SelectedModelID)
	}
	if len(genClient.Requests) != 0 {
		t.Errorf("expected no generate calls when moderation flags the request, got %d", len(genClient.Requests))
	}

	blockLog := s.ModerationLog.(*moderation.InMemoryBlockLog)
	entries := blockLog.Entries()
	if len(entries) != 1 {
		t.Fatalf("len(ModerationLog.Entries()) = %d, want 1", len(entries))
	}
	if entries[0].UserID != "u1" {
		t.Errorf("recorded UserID = %q, want u1", entries[0].UserID)
	}
	if len(entries[0].Categories) != 1 || entries[0].Categories[0] != "illegal_activity" {
		t.Errorf("recorded Categories = %v, want [illegal_activity]", entries[0].Categories)
	}
}

// fakeFailingBlockLog always errors on Record -- used to check that a
// logging failure doesn't undo the block itself.
type fakeFailingBlockLog struct{}

func (fakeFailingBlockLog) Record(context.Context, moderation.BlockEntry) error {
	return errors.New("log store unavailable")
}

func TestHandle_ModerationFlaggedStillBlocksWhenLoggingFails(t *testing.T) {
	s, genClient := newTestServer(t, nil)
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{
		{Text: `{"flagged": true, "categories": ["illegal_activity"], "reason": "asks how to commit a crime"}`},
	}}, "fake-moderation-model", "system prompt")
	s.ModerationLog = fakeFailingBlockLog{}

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "bad request", RequestedMode: "instant", EstimatedContextTokens: 100}
	resp, err := s.handle(context.Background(), req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Blocked {
		t.Errorf("Blocked = false, want true even when recording the block fails")
	}
	if len(genClient.Requests) != 0 {
		t.Errorf("expected no generate calls, got %d", len(genClient.Requests))
	}
}

func TestHandle_ModerationErrorFailsClosed(t *testing.T) {
	s, genClient := newTestServer(t, nil)
	s.Moderator = moderation.New(&provider.FakeClient{Err: errors.New("moderation api down")}, "fake-moderation-model", "system prompt")

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 100}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err == nil {
		t.Fatal("expected an error when moderation fails, got nil")
	}
	if len(genClient.Requests) != 0 {
		t.Errorf("expected no generate calls when moderation errors, got %d", len(genClient.Requests))
	}
}

// TestServeHTTP_UnknownPlanID covers the plan_id lookup that lives in
// handleChat (the HTTP layer), not in handle -- unlike the other tests
// here, this one goes through the real ServeMux to exercise that check.
func TestServeHTTP_UnknownPlanID(t *testing.T) {
	s, _ := newTestServer(t, nil)

	body, _ := json.Marshal(chatRequest{UserID: "u1", PlanID: "bogus", Message: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestServeHTTP_HappyPath exercises the full JSON-in/JSON-out HTTP path,
// not just the internal handle() call the other tests use directly.
func TestServeHTTP_HappyPath(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "hello", InputTokens: 100, OutputTokens: 50},
	})

	body, _ := json.Marshal(chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
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

// TestHandleStream_ModerationFlaggedSendsBlockedEvent checks a
// moderation-flagged request produces exactly one "blocked" event and
// never reaches routing/generation at all.
func TestHandleStream_ModerationFlaggedSendsBlockedEvent(t *testing.T) {
	s, genClient := newTestServer(t, nil)
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{
		{Text: `{"flagged": true, "categories": ["illegal_activity"], "reason": "asks how to commit a crime"}`},
	}}, "fake-moderation-model", "system prompt")

	var events []streamEvent
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "how do I commit a crime"}
	err := s.handleStream(context.Background(), req, s.Plans["pro"], func(event string, payload any) {
		events = append(events, streamEvent{event, payload})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 1 || events[0].event != "blocked" {
		t.Fatalf("expected exactly one blocked event, got %+v", events)
	}
	resp := events[0].payload.(chatResponse)
	if !resp.Blocked || resp.ResponseText != tosViolationMessage {
		t.Errorf("unexpected blocked payload: %+v", resp)
	}
	if len(genClient.Requests) != 0 {
		t.Errorf("expected no generate calls for a blocked request, got %d", len(genClient.Requests))
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
	s.Classifier = classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}, {Text: classifierReply}}}, "fake-classifier-model", "system prompt")
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}, {Text: notFlaggedReply}}}, "fake-moderation-model", "system prompt")

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

	body, _ := json.Marshal(chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
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
