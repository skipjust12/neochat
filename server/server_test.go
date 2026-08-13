package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"neochat/classifier"
	"neochat/conversation"
	"neochat/costlog"
	"neochat/idempotency"
	"neochat/limits"
	"neochat/moderation"
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
		Idempotency:   idempotency.NewInMemoryStore(),
		CostLog:       costlog.NewInMemoryStore(),
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

// TestHandle_RecordsCostLogEntry checks that a successful generation logs
// a costlog.Store record with the real token usage/cost -- distinct from
// Store's rolling-window spend accounting (checked above), this is the
// per-request record for billing reconciliation and unit-economics
// tracking (README pre-launch checklist item 7). newTestServer's
// classifier/moderator also log their own entries (see
// TestHandle_LogsClassifyAndModerateCostEntries) -- this test picks out
// the generation one specifically by Mode.
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

// findCostLogEntry returns the first entry with the given Mode, and
// whether one was found -- helper for tests that need to pick one
// specific call's record out of a /chat request's several (classify,
// moderate, generate).
func findCostLogEntry(entries []router.CostLogEntry, mode string) (router.CostLogEntry, bool) {
	for _, e := range entries {
		if e.Mode == mode {
			return e, true
		}
	}
	return router.CostLogEntry{}, false
}

// TestHandle_LogsClassifyAndModerateCostEntries checks that both the
// classifier and moderation calls log their own cost_log entry -- README
// pre-launch checklist item 7's previously-open "classifier and
// moderation calls" gap -- sharing the same RequestID as the eventual
// generation entry so all three of one /chat call's billed model calls
// tie together.
func TestHandle_LogsClassifyAndModerateCostEntries(t *testing.T) {
	s, _ := newTestServer(t, []provider.GenerateResult{
		{Text: "here is the answer", InputTokens: 1000, OutputTokens: 500},
	})
	s.Classifier = classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{
		{Text: classifierReply, InputTokens: 1570, OutputTokens: 150},
	}}, "fake-classifier-model", "system prompt")
	s.Classifier.CostInputPerMTok, s.Classifier.CostOutputPerMTok = 0.3, 2.5
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{
		{Text: notFlaggedReply, InputTokens: 170, OutputTokens: 20},
	}}, "fake-moderation-model", "system prompt")
	s.Moderator.CostInputPerMTok, s.Moderator.CostOutputPerMTok = 0.03, 0.17

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "explain recursion", RequestedMode: "thinking", EstimatedContextTokens: 2000}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := s.CostLog.(*costlog.InMemoryStore).Entries()
	if len(entries) != 3 {
		t.Fatalf("len(CostLog.Entries()) = %d, want 3 (classify, moderate, generate)", len(entries))
	}

	classifyEntry, ok := findCostLogEntry(entries, "classify")
	if !ok {
		t.Fatalf("no cost_log entry with Mode=classify among %+v", entries)
	}
	if classifyEntry.ModelID != "fake-classifier-model" {
		t.Errorf("classify ModelID = %q, want fake-classifier-model", classifyEntry.ModelID)
	}
	if classifyEntry.CostUSD <= 0 {
		t.Errorf("classify CostUSD = %.6f, want > 0", classifyEntry.CostUSD)
	}

	moderateEntry, ok := findCostLogEntry(entries, "moderate")
	if !ok {
		t.Fatalf("no cost_log entry with Mode=moderate among %+v", entries)
	}
	if moderateEntry.ModelID != "fake-moderation-model" {
		t.Errorf("moderate ModelID = %q, want fake-moderation-model", moderateEntry.ModelID)
	}
	if moderateEntry.CostUSD <= 0 {
		t.Errorf("moderate CostUSD = %.6f, want > 0", moderateEntry.CostUSD)
	}

	generateEntry, ok := findCostLogEntry(entries, "thinking")
	if !ok {
		t.Fatalf("no cost_log entry with Mode=thinking among %+v", entries)
	}
	if classifyEntry.RequestID == "" || classifyEntry.RequestID != moderateEntry.RequestID || classifyEntry.RequestID != generateEntry.RequestID {
		t.Errorf("expected all three entries to share one RequestID, got classify=%q moderate=%q generate=%q",
			classifyEntry.RequestID, moderateEntry.RequestID, generateEntry.RequestID)
	}
}

// TestHandle_LogsClassifyAndModerateCostEntriesWhenModerationBlocks checks
// that a moderation-flagged request -- which never reaches generation --
// still logs the classify/moderate entries, since both calls happened
// (and were billed) regardless of the verdict. Mirrors
// TestHandle_ModerationFlaggedBlocksBeforeGeneration's "no generate calls"
// assertion, one layer further down the pipeline: no generation entry,
// but the two upstream ones are still expected.
func TestHandle_LogsClassifyAndModerateCostEntriesWhenModerationBlocks(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{
		{Text: `{"flagged": true, "categories": ["illegal_activity"], "reason": "asks how to commit a crime"}`, InputTokens: 170, OutputTokens: 20},
	}}, "fake-moderation-model", "system prompt")
	s.Moderator.CostInputPerMTok, s.Moderator.CostOutputPerMTok = 0.03, 0.17

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "bad request", RequestedMode: "instant", EstimatedContextTokens: 100}
	if _, err := s.handle(context.Background(), req, s.Plans["pro"]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := s.CostLog.(*costlog.InMemoryStore).Entries()
	if len(entries) != 2 {
		t.Fatalf("len(CostLog.Entries()) = %d, want 2 (classify + moderate, no generation)", len(entries))
	}
	if _, ok := findCostLogEntry(entries, "classify"); !ok {
		t.Errorf("no cost_log entry with Mode=classify among %+v", entries)
	}
	moderateEntry, ok := findCostLogEntry(entries, "moderate")
	if !ok {
		t.Fatalf("no cost_log entry with Mode=moderate among %+v", entries)
	}
	if moderateEntry.InputTokens != 170 || moderateEntry.OutputTokens != 20 {
		t.Errorf("moderate tokens = %d/%d, want 170/20", moderateEntry.InputTokens, moderateEntry.OutputTokens)
	}
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
	for _, m := range genClient.Requests[0].Messages {
		if m.Role == "system" {
			t.Errorf("expected no system message for a persona with no loaded prompt, got %+v", genClient.Requests[0].Messages)
		}
	}
}

// TestServeHTTP_UnknownPersonaRejected checks an unrecognized persona name
// (a typo, or a client not yet updated to a persona list change) is
// rejected up front with 400, the same way an unknown plan_id is --
// before any classify/moderate/route/generate work happens.
func TestServeHTTP_UnknownPersonaRejected(t *testing.T) {
	s, genClient := newTestServer(t, nil)

	body, _ := json.Marshal(chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", Persona: "Snarky"})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if len(genClient.Requests) != 0 {
		t.Errorf("expected no generate calls for a rejected request, got %d", len(genClient.Requests))
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

// TestHandle_InstantCapExceededRejectsBeforeClassifyOrModerate checks that
// once a user_id's Instant-pool spend has crossed their plan's
// InstantExtraCapUSD anti-bot ceiling, handle rejects the request with
// ErrInstantCapExceeded before running classify/moderate at all -- both
// are themselves billed calls, so an over-cap request should never reach
// them (see prepare's doc comment on ErrInstantCapExceeded).
func TestHandle_InstantCapExceededRejectsBeforeClassifyOrModerate(t *testing.T) {
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

	classifierClient := s.Classifier.Client.(*provider.FakeClient)
	if len(classifierClient.Requests) != 0 {
		t.Errorf("classifier was called %d times, want 0 (over-cap request must reject before classify)", len(classifierClient.Requests))
	}
	moderationClient := s.Moderator.Client.(*provider.FakeClient)
	if len(moderationClient.Requests) != 0 {
		t.Errorf("moderator was called %d times, want 0 (over-cap request must reject before moderate)", len(moderationClient.Requests))
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

	body, _ := json.Marshal(chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
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
		UserID: "u1", PlanID: "pro",
		Message: strings.Repeat("a", maxRequestBodyBytes+1),
	}
	body, _ := json.Marshal(oversized)
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
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

	body, _ := json.Marshal(chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})

	req1 := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec1 := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st request status = %d, want %d, body = %s", rec1.Code, http.StatusOK, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec2 := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("2nd request status = %d, want %d, body = %s", rec2.Code, http.StatusTooManyRequests, rec2.Body.String())
	}

	if len(genClient.Requests) != 1 {
		t.Errorf("generator was called %d times, want 1 (2nd request must be rejected before reaching it)", len(genClient.Requests))
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

// TestServeHTTP_InternalErrorDoesNotLeakDetails checks that a failure
// inside the pipeline (here, moderation's vendor call erroring out) never
// reaches the HTTP caller as raw error text -- only a generic message,
// with the real error only in the server log (audit.md's error-leakage
// finding). "moderation api down" standing in for anything an internal
// error could carry: vendor response text, model IDs, database/Redis
// failure details.
func TestServeHTTP_InternalErrorDoesNotLeakDetails(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.Moderator = moderation.New(&provider.FakeClient{Err: errors.New("moderation api down")}, "fake-moderation-model", "system prompt")

	body, _ := json.Marshal(chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := rec.Body.String(); strings.Contains(got, "moderation api down") {
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

	body, _ := json.Marshal(chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "instant", EstimatedContextTokens: 500})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	s.Mux().ServeHTTP(rec, req)

	if got := rec.Body.String(); !strings.Contains(got, "instant-tier spend cap exceeded") {
		t.Errorf("response body = %q, want it to contain ErrInstantCapExceeded's own message", got)
	}
}

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

	if second != first {
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
	if spent != wantCost {
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
	// Two handle() calls means the classifier/moderator fakes need two
	// scripted replies each, not newTestServer's default one.
	s.Classifier = classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}, {Text: classifierReply}}}, "fake-classifier-model", "system prompt")
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}, {Text: notFlaggedReply}}}, "fake-moderation-model", "system prompt")

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
	// Two handle() calls means the classifier/moderator fakes need two
	// scripted replies each, not newTestServer's default one.
	s.Classifier = classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}, {Text: classifierReply}}}, "fake-classifier-model", "system prompt")
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}, {Text: notFlaggedReply}}}, "fake-moderation-model", "system prompt")

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
	// Two handle() calls means the classifier/moderator fakes need two
	// scripted replies each, not newTestServer's default one.
	s.Classifier = classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}, {Text: classifierReply}}}, "fake-classifier-model", "system prompt")
	s.Moderator = moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}, {Text: notFlaggedReply}}}, "fake-moderation-model", "system prompt")

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
	if second[0].payload.(chatResponse) != firstDone {
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
	prepared, blocked, err := s.prepare(ctx, req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked != nil {
		t.Fatalf("unexpected block: %+v", blocked)
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
		Router:        router.NewRouter(catalog, testWeights()),
		Classifier:    classifier.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: classifierReply}}}, "fake-classifier-model", "system prompt"),
		Moderator:     moderation.New(&provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}}}, "fake-moderation-model", "system prompt"),
		ModerationLog: moderation.NewInMemoryBlockLog(),
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
