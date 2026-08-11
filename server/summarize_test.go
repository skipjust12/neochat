package server

import (
	"context"
	"testing"

	"neochat/conversation"
	"neochat/provider"
	"neochat/summarizer"
	"neochat/tokenizer"
)

// longHistory returns n alternating user/assistant messages, each long
// enough on its own to push tokenizer.EstimateMessages well past a small
// trigger threshold once there are several of them.
func longHistory(n int) []conversation.Message {
	history := make([]conversation.Message, n)
	for i := range history {
		role := conversation.RoleUser
		if i%2 == 1 {
			role = conversation.RoleAssistant
		}
		history[i] = conversation.Message{Role: role, Content: "this is message content long enough to add up in tokens"}
	}
	return history
}

func TestMaybeSummarize_DisabledWhenTriggerTokensUnset(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// SummaryTriggerTokens left at its zero value -- feature is off.

	got, summaryText := s.maybeSummarize(context.Background(), "u1", "c1", longHistory(30))
	if summaryText != "" {
		t.Errorf("summaryText = %q, want empty when the feature is unconfigured", summaryText)
	}
	if len(got) != 30 {
		t.Errorf("len(got) = %d, want 30 (full history returned unchanged)", len(got))
	}
}

func TestMaybeSummarize_BelowThresholdReturnsHistoryUnchanged(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.SummaryTriggerTokens = 1_000_000 // unreachable
	s.SummaryTailMessages = 4
	fakeSummarizer := &provider.FakeClient{}
	s.Summarizer = summarizer.New(fakeSummarizer, "fake-summarizer-model", "system prompt")

	history := longHistory(10)
	got, summaryText := s.maybeSummarize(context.Background(), "u1", "c1", history)
	if summaryText != "" {
		t.Errorf("summaryText = %q, want empty below the trigger threshold", summaryText)
	}
	if len(got) != len(history) {
		t.Errorf("len(got) = %d, want %d (full history)", len(got), len(history))
	}
	if len(fakeSummarizer.Requests) != 0 {
		t.Errorf("expected the summarizer not to be called, got %d requests", len(fakeSummarizer.Requests))
	}
}

func TestMaybeSummarize_AboveThresholdFoldsOlderMessagesAndKeepsTail(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.SummaryTriggerTokens = 1 // anything past the tail-only check trips this
	s.SummaryTailMessages = 4
	fakeSummarizer := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: "folded summary"}}}
	s.Summarizer = summarizer.New(fakeSummarizer, "fake-summarizer-model", "system prompt")

	history := longHistory(10)
	got, summaryText := s.maybeSummarize(context.Background(), "u1", "c1", history)

	if summaryText != "folded summary" {
		t.Errorf("summaryText = %q, want %q", summaryText, "folded summary")
	}
	if len(got) != s.SummaryTailMessages {
		t.Fatalf("len(got) = %d, want %d (the tail)", len(got), s.SummaryTailMessages)
	}
	wantTail := history[len(history)-s.SummaryTailMessages:]
	for i := range got {
		if got[i].Content != wantTail[i].Content {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], wantTail[i])
		}
	}

	if len(fakeSummarizer.Requests) != 1 {
		t.Fatalf("expected exactly 1 summarizer call, got %d", len(fakeSummarizer.Requests))
	}

	stored, err := s.Conversations.GetSummary(context.Background(), "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCoversThrough := len(history) - s.SummaryTailMessages
	if stored.Text != "folded summary" || stored.CoversThrough != wantCoversThrough {
		t.Errorf("stored summary = %+v, want Text=%q CoversThrough=%d", stored, "folded summary", wantCoversThrough)
	}
}

func TestMaybeSummarize_ReusesStoredSummaryWhenNothingNewToFold(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.SummaryTriggerTokens = 1
	s.SummaryTailMessages = 4
	fakeSummarizer := &provider.FakeClient{}
	s.Summarizer = summarizer.New(fakeSummarizer, "fake-summarizer-model", "system prompt")

	history := longHistory(10)
	tailStart := len(history) - s.SummaryTailMessages
	must(t, s.Conversations.SetSummary(context.Background(), "u1", "c1", conversation.Summary{
		Text:          "already up to date",
		CoversThrough: tailStart,
	}))

	got, summaryText := s.maybeSummarize(context.Background(), "u1", "c1", history)

	if summaryText != "already up to date" {
		t.Errorf("summaryText = %q, want the already-stored summary reused as-is", summaryText)
	}
	if len(got) != s.SummaryTailMessages {
		t.Errorf("len(got) = %d, want %d", len(got), s.SummaryTailMessages)
	}
	if len(fakeSummarizer.Requests) != 0 {
		t.Errorf("expected no summarizer call when there's nothing new to fold, got %d", len(fakeSummarizer.Requests))
	}
}

func TestMaybeSummarize_SummarizerErrorFallsBackToFullHistory(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.SummaryTriggerTokens = 1
	s.SummaryTailMessages = 4
	fakeSummarizer := &provider.FakeClient{Err: context.DeadlineExceeded}
	s.Summarizer = summarizer.New(fakeSummarizer, "fake-summarizer-model", "system prompt")

	history := longHistory(10)
	got, summaryText := s.maybeSummarize(context.Background(), "u1", "c1", history)

	if summaryText != "" {
		t.Errorf("summaryText = %q, want empty on summarizer failure", summaryText)
	}
	if len(got) != len(history) {
		t.Errorf("len(got) = %d, want %d (fall back to full history)", len(got), len(history))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPrepare_FoldsHistoryIntoSummaryWhenOverTrigger is the end-to-end
// check (not just maybeSummarize in isolation): with a real, pre-existing
// conversation stored via s.Conversations and SummaryTriggerTokens set low
// enough that its estimated size is over the threshold, prepare's actual
// output -- the message list it's about to hand to the provider, and its
// own estimatedContextTokens -- reflects the summary + tail, not the full
// raw history.
func TestPrepare_FoldsHistoryIntoSummaryWhenOverTrigger(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.SummaryTriggerTokens = 50 // low enough that 20 real stored messages trip it
	s.SummaryTailMessages = 4
	fakeSummarizer := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: "SUMMARY: earlier the user and assistant discussed several topics."}}}
	s.Summarizer = summarizer.New(fakeSummarizer, "fake-summarizer-model", "system prompt")

	ctx := context.Background()
	conversationID := "c1"
	for _, m := range longHistory(20) {
		must(t, s.Conversations.Append(ctx, "u1", conversationID, m))
	}

	// Sanity check: the full stored history really is over the trigger
	// threshold before asserting prepare acted on that.
	full, err := s.Conversations.History(ctx, "u1", conversationID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fullAsProvider := make([]provider.Message, len(full))
	for i, m := range full {
		fullAsProvider[i] = provider.Message{Role: string(m.Role), Content: m.Content}
	}
	fullEstimate := tokenizer.EstimateMessages(fullAsProvider)
	if fullEstimate <= s.SummaryTriggerTokens {
		t.Fatalf("test setup bug: full history estimate %d is not over SummaryTriggerTokens %d", fullEstimate, s.SummaryTriggerTokens)
	}
	t.Logf("full history estimate = %d tokens, SummaryTriggerTokens = %d", fullEstimate, s.SummaryTriggerTokens)

	req := chatRequest{UserID: "u1", PlanID: "pro", ConversationID: conversationID, Message: "one more question", RequestedMode: "instant"}
	prepared, blocked, err := s.prepare(ctx, req, s.Plans["pro"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked != nil {
		t.Fatalf("unexpected block: %+v", blocked)
	}

	if len(fakeSummarizer.Requests) != 1 {
		t.Fatalf("expected the summarizer to be called exactly once, got %d calls", len(fakeSummarizer.Requests))
	}

	var sawSummary bool
	for _, m := range prepared.messages {
		if m.Role == "system" && m.Content == "Earlier in this conversation:\nSUMMARY: earlier the user and assistant discussed several topics." {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Errorf("expected a system message carrying the summary text, got messages: %+v", prepared.messages)
	}

	// tail (4) + summary system message + new user message == 6, versus
	// 20 raw history + 1 new message == 21 without summarization.
	if len(prepared.messages) != s.SummaryTailMessages+2 {
		t.Errorf("len(prepared.messages) = %d, want %d (summary system message + %d tail + new user message)", len(prepared.messages), s.SummaryTailMessages+2, s.SummaryTailMessages)
	}

	if prepared.estimatedContextTokens >= fullEstimate {
		t.Errorf("estimatedContextTokens = %d, want less than the full-history estimate %d -- summarization should have shrunk it", prepared.estimatedContextTokens, fullEstimate)
	}
	t.Logf("prepared.estimatedContextTokens = %d (down from full-history %d)", prepared.estimatedContextTokens, fullEstimate)
}
