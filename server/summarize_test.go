package server

import (
	"context"
	"testing"

	"neochat/conversation"
	"neochat/provider"
	"neochat/summarizer"
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
