package server

import (
	"context"
	"errors"
	"math"
	"neochat/classifier"
	"neochat/conversation"
	"neochat/costlog"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/router"
	"neochat/summarizer"
	"testing"
	"time"
)

func securityFixture() *Server {
	model := router.Model{ID: "instant", Provider: "openai", APIModelID: "instant", Modes: []string{"instant"}, CostInputPerMTok: 1, CostOutputPerMTok: 2, ContextWindow: 50000, MaxOutputTokens: 1000, SupportsModality: []string{"text"}}
	return &Server{
		Router: router.NewRouter(router.Catalog{Models: []router.Model{model}}, router.Weights{}),
		Classifier: classifier.New(&provider.FakeClient{PerModel: map[string]provider.GenerateResult{"classify": {Text: `{
	"schema_version": "1.1", "task_category": "general", "task_intent": "answer", "language": "en",
	"modality_input": ["text"], "modality_output_expected": ["text"],
	"reasoning_depth": "high", "creativity_level": "low", "required_tools": [],
	"expected_output_length": "medium", "estimated_output_tokens": 300,
	"output_format": "", "complexity_score": 0.8, "context_dependency": "light",
	"confidence": 0.9, "content_flags": [], "safety_risk_score": 0.0
}`, InputTokens: 100, OutputTokens: 100}}}, "classify", "system"),
		Moderator:     moderation.New(&provider.FakeClient{PerModel: map[string]provider.GenerateResult{"moderate": {Text: `{"flagged":false,"categories":[],"reason":""}`, InputTokens: 100, OutputTokens: 100}}}, "moderate", "system"),
		ModerationLog: moderation.NewInMemoryBlockLog(), Conversations: conversation.NewInMemoryStore(), Store: limits.NewInMemorySpendStore(), CostLog: costlog.NewInMemoryStore(),
		Generators: map[string]provider.Client{"openai": &provider.FakeClient{PerModel: map[string]provider.GenerateResult{"instant": {Text: "answer", InputTokens: 20, OutputTokens: 1000}}}},
	}
}

func TestSecurityManualInstantChargesAndChecksSamePool(t *testing.T) {
	s := securityFixture()
	ctx := context.Background()
	plan := limits.PlanLimits{ThinkingMaxCapUSD: 0.001, InstantExtraCapUSD: 0.0041}
	if err := s.Store.Record(ctx, "user", limits.PoolThinkingMax, 0.001, time.Now()); err != nil {
		t.Fatal(err)
	}
	req := chatRequest{UserID: "user", Message: "hello", RequestedMode: "manual", ManualModelID: "instant"}
	if _, err := s.handle(ctx, req, plan); err != nil {
		t.Fatal(err)
	}
	spent, _ := s.Store.Sum(ctx, "user", limits.PoolInstant, 30*24*time.Hour)
	if spent <= 0 {
		t.Fatal("manual Instant was not charged to Instant")
	}
	if _, err := s.handle(ctx, req, plan); !errors.Is(err, limits.ErrBudgetExceeded) {
		t.Fatalf("budget bypass: %v", err)
	}
	if calls := len(s.Generators["openai"].(*provider.FakeClient).Requests); calls != 1 {
		t.Fatalf("%d generations, want 1", calls)
	}
}

func TestSecurityInvalidModeDoesNotCallVendors(t *testing.T) {
	s := securityFixture()
	_, err := s.handle(context.Background(), chatRequest{UserID: "user", Message: "hello", RequestedMode: "invalid"}, limits.PlanLimits{InstantExtraCapUSD: 1})
	if !errors.Is(err, errInvalidRequest) {
		t.Fatalf("invalid mode: %v", err)
	}
	if len(s.Classifier.Client.(*provider.FakeClient).Requests) != 0 || len(s.Moderator.Client.(*provider.FakeClient).Requests) != 0 {
		t.Fatal("paid calls before validation")
	}
}

func TestSecurityModerationBlockStillConsumesBudget(t *testing.T) {
	s := securityFixture()
	ctx := context.Background()
	s.Classifier.CostInputPerMTok = 1
	s.Classifier.CostOutputPerMTok = 1
	s.Moderator.CostInputPerMTok = 1
	s.Moderator.CostOutputPerMTok = 1
	s.Moderator.Client.(*provider.FakeClient).PerModel["moderate"] = provider.GenerateResult{Text: `{"flagged":true,"categories":[],"reason":"blocked"}`, InputTokens: 100, OutputTokens: 100}
	plan := limits.PlanLimits{InstantExtraCapUSD: 0.1, ThinkingMaxCapUSD: 1}
	req := chatRequest{UserID: "user", Message: "hello", RequestedMode: "instant"}
	response, err := s.handle(ctx, req, plan)
	if err != nil || !response.Blocked {
		t.Fatalf("block: %+v %v", response, err)
	}
	spent, _ := s.Store.Sum(ctx, "user", limits.PoolInstant, 30*24*time.Hour)
	if math.Abs(spent-0.0004) > 1e-12 {
		t.Fatalf("missing auxiliary charges: %.9f", spent)
	}
	plan.InstantExtraCapUSD = spent
	if _, err := s.handle(ctx, req, plan); !errors.Is(err, ErrInstantCapExceeded) {
		t.Fatalf("block loop bypass: %v", err)
	}
}

func TestSecurityCancellationKeepsReservation(t *testing.T) {
	s := securityFixture()
	ctx, cancel := context.WithCancel(context.Background())
	req := chatRequest{UserID: "user"}
	plan := limits.PlanLimits{InstantExtraCapUSD: 0.001}
	_, err := s.billedCall(ctx, req, plan, limits.PoolInstant, 1, 1, 100, nil, "model", nil, func(ctx context.Context, _ provider.Client, _ string, _ []provider.Message) (provider.GenerateResult, error) {
		cancel()
		return provider.GenerateResult{}, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	spent, _ := s.Store.Sum(context.Background(), "user", limits.PoolInstant, 30*24*time.Hour)
	if spent <= 0 {
		t.Fatal("cancellation restored the budget without usage")
	}
}

func TestSecurityFinalUsageSettlesDespiteClientDisconnect(t *testing.T) {
	s := securityFixture()
	ctx, cancel := context.WithCancel(context.Background())
	req := chatRequest{UserID: "user"}
	plan := limits.PlanLimits{InstantExtraCapUSD: 1}
	_, err := s.billedCall(ctx, req, plan, limits.PoolInstant, 1, 1, 100, nil, "model", nil, func(context.Context, provider.Client, string, []provider.Message) (provider.GenerateResult, error) {
		cancel()
		return provider.GenerateResult{Text: "ok", InputTokens: 20, OutputTokens: 10}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	spent, _ := s.Store.Sum(context.Background(), "user", limits.PoolInstant, 30*24*time.Hour)
	if math.Abs(spent-0.00003) > 1e-12 {
		t.Fatalf("lost final usage: %f", spent)
	}
}

func TestSecuritySummaryConsumesBudget(t *testing.T) {
	s := securityFixture()
	ctx := context.Background()
	s.SummaryTriggerTokens = 1
	s.SummaryTailMessages = 1
	s.Summarizer = summarizer.New(&provider.FakeClient{PerModel: map[string]provider.GenerateResult{"summary": {Text: "summary", InputTokens: 20, OutputTokens: 10}}}, "summary", "system")
	s.Summarizer.CostInputPerMTok = 1
	s.Summarizer.CostOutputPerMTok = 1
	for i := 0; i < 3; i++ {
		if err := s.Conversations.Append(ctx, "user", "conversation", conversation.Message{Role: conversation.RoleUser, Content: "old message"}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := s.prepare(ctx, chatRequest{UserID: "user", ConversationID: "conversation", Message: "hello", RequestedMode: "instant"}, limits.PlanLimits{InstantExtraCapUSD: 1, ThinkingMaxCapUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	spent, _ := s.Store.Sum(ctx, "user", limits.PoolInstant, 30*24*time.Hour)
	if math.Abs(spent-0.00003) > 1e-12 {
		t.Fatalf("summary not billed: %f", spent)
	}
}
