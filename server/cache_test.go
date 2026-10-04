package server

import (
	"context"
	"math"
	"testing"
	"time"

	"neochat/limits"
	"neochat/provider"
)

// cacheMarked reports which messages of a vendor request carry a
// cache_control mark, by role and plain text.
func cacheMarked(body map[string]any) []string {
	var marked []string
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		m := raw.(map[string]any)
		parts, _ := m["content"].([]any)
		for _, rawPart := range parts {
			part := rawPart.(map[string]any)
			if part["cache_control"] != nil {
				marked = append(marked, m["role"].(string)+": "+part["text"].(string))
			}
		}
	}
	return marked
}

func TestPromptCache_ClaudeChatMarksPrefixAndBillsWhatPolzaCharged(t *testing.T) {
	standIn := &polzaStandIn{reply: "answer", usage: `{"prompt_tokens":3000,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2800,"cache_write_tokens":150},"cost_rub":1.17068}`}
	s := newDirectServer(t, standIn)
	ctx := context.Background()
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "first question", RequestedMode: "manual", ManualModelID: "claude-haiku-4-5", Persona: "Expert", Instructions: "Be brief.", providerKey: "pza_user"}
	events, err := collectStream(s, ctx, req)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	first := events["done"][0].(chatResponse)

	req.Message, req.ConversationID = "second question", first.ConversationID
	events, err = collectStream(s, ctx, req)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	second := events["done"][0].(chatResponse)

	marked := cacheMarked(standIn.lastBody(t))
	want := []string{"system: The user's custom instructions (from their settings) -- follow them unless they conflict with the rules above:\nBe brief.", "user: first question", "user: second question"}
	if len(marked) != len(want) {
		t.Fatalf("cache marks = %q, want %q", marked, want)
	}
	for i := range want {
		if marked[i] != want[i] {
			t.Fatalf("cache marks = %q, want %q", marked, want)
		}
	}

	// 1.17068 RUB at 117.068 RUB/USD.
	if math.Abs(second.ActualCostUSD-0.01) > 1e-9 {
		t.Fatalf("actual cost = %v USD, want 0.01 (what Polza charged)", second.ActualCostUSD)
	}
	spent, _ := s.Store.Sum(ctx, "u1", limits.PoolInstant, time.Hour)
	if math.Abs(spent-0.02) > 1e-9 {
		t.Fatalf("spent = %v USD over two turns, want 0.02", spent)
	}
}

func TestPromptCache_OtherVendorsAreLeftToCacheOnTheirOwn(t *testing.T) {
	standIn := &polzaStandIn{reply: "answer"}
	s := newDirectServer(t, standIn)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "question", RequestedMode: "manual", ManualModelID: "gpt-6-luna", Persona: "Expert", providerKey: "pza_user"}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if marked := cacheMarked(standIn.lastBody(t)); len(marked) != 0 {
		t.Fatalf("cache marks = %q, want none", marked)
	}
}

func TestCallCostUSD(t *testing.T) {
	// $1 / $2 per million tokens.
	cases := []struct {
		name   string
		result provider.GenerateResult
		want   float64
	}{
		{"what Polza charged", provider.GenerateResult{InputTokens: 1_000_000, OutputTokens: 1_000_000, CachedTokens: 900_000, CostRUB: 117.068}, 1},
		{"list price", provider.GenerateResult{InputTokens: 1_000_000, OutputTokens: 1_000_000}, 3},
		{"cache reads counted in full, writes at 1.25x", provider.GenerateResult{InputTokens: 1_000_000, OutputTokens: 0, CachedTokens: 500_000, CacheWriteTokens: 400_000}, 1.1},
	}
	for _, tc := range cases {
		if got := callCostUSD(1, 2, tc.result); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: cost = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWebAnswerAdd_SumsUsageAndDropsAPartialCharge(t *testing.T) {
	var answer webAnswer
	answer.add(provider.GenerateResult{InputTokens: 100, OutputTokens: 10, CachedTokens: 80, CacheWriteTokens: 5, CostRUB: 1})
	answer.add(provider.GenerateResult{InputTokens: 200, OutputTokens: 20, CachedTokens: 150, CostRUB: 2})
	if answer.InputTokens != 300 || answer.OutputTokens != 30 || answer.CachedTokens != 230 || answer.CacheWriteTokens != 5 || answer.CostRUB != 3 {
		t.Fatalf("summed usage = %+v", answer.GenerateResult)
	}
	// A call that doesn't say what it cost leaves the answer priced by
	// tokens, and so do the calls after it.
	answer.add(provider.GenerateResult{InputTokens: 10, OutputTokens: 1})
	answer.add(provider.GenerateResult{InputTokens: 10, OutputTokens: 1, CostRUB: 5})
	if answer.CostRUB != 0 {
		t.Fatalf("cost = %v RUB after an unpriced call, want 0", answer.CostRUB)
	}
}
