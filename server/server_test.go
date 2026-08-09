package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"neochat/classifier"
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
		Router:     router.NewRouter(testCatalog(), testWeights()),
		Classifier: classifier.New(classifierClient, "fake-classifier-model", "system prompt"),
		Moderator:  moderation.New(moderationClient, "fake-moderation-model", "system prompt"),
		Store:      limits.NewInMemorySpendStore(),
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
