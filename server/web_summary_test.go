package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"neochat/costlog"
	"neochat/provider"
	"neochat/summarizer"
)

func summaryRun(t *testing.T) (*webRun, []provider.Message, *provider.FakeClient) {
	t.Helper()
	r := newBatchRun(t)
	r.req.SearchMode = true
	r.req.providerKey = "user-poe-key"
	r.searchNotes = "optional working notes"
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: `{"summaries":[{"id":"old","summary":"The official report confirms total 12345 on 2026-11-27; a secondary source disagrees. https://example.com/report"}]}`, InputTokens: 9000, OutputTokens: 100}}}
	r.s.Summarizer = summarizer.New(fake, "gpt-6-luna", "conversation summary prompt must not be used here")
	r.webRecords = map[string]webRecord{
		"old":   {round: 1, compact: "https://example.com/report\nhttps://example.com/disagreement"},
		"fresh": {round: 5, compact: "https://example.com/fresh"},
	}
	messages := []provider.Message{
		{Role: "assistant", ToolCalls: []provider.ToolCall{batchTool("old", "web_search", map[string]any{"queries": []string{"report"}, "notes": "existing observations must survive"})}},
		{Role: "tool", ToolCallID: "old", Content: strings.Repeat("background ", 1800) + "\nEXACT FINDING: total 12345 on 2026-11-27. https://example.com/report. Disagreement: https://example.com/disagreement"},
		{Role: "tool", ToolCallID: "fresh", Content: strings.Repeat("recent evidence ", 1000)},
		{Role: "tool", ToolCallID: "script", Content: "script output"},
	}
	return r, messages, fake
}

func TestWebSummaryPreservesRawUntilBothAgeAndBudget(t *testing.T) {
	r, messages, fake := summaryRun(t)
	for _, round := range []int{2, 3, 4, 5} {
		got := r.compactWebMessages(context.Background(), messages, round)
		if got[1].Content != messages[1].Content || len(fake.Requests) != 0 {
			t.Fatalf("premature summarization at round %d", round)
		}
	}
	small := append([]provider.Message(nil), messages...)
	small[1].Content = "known exact fact 12345"
	small[2].Content = "another source"
	if got := r.compactWebMessages(context.Background(), small, 20); got[1].Content != small[1].Content || len(fake.Requests) != 0 {
		t.Fatal("small evidence was erased just because it aged")
	}
	r.req.SearchMode = false
	if got := r.compactWebMessages(context.Background(), messages, 9); got[1].Content != messages[1].Content || len(fake.Requests) != 0 {
		t.Fatal("normal mode ignored larger budget")
	}
}

func TestWebSummaryUsesFullEvidencePreservesSourcesAndReusesDigest(t *testing.T) {
	r, messages, fake := summaryRun(t)
	got := r.compactWebMessages(context.Background(), messages, 6)
	if len(fake.Requests) != 1 {
		t.Fatal("expected one batch helper call")
	}
	call := fake.Requests[0]
	if call.APIModelID != "gpt-6-luna" || call.Messages[0].Content != webEvidenceSummaryPrompt {
		t.Fatal("wrong summarizer or prompt")
	}
	var body struct {
		Request string                 `json:"request"`
		Outputs []evidenceSummaryInput `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(call.Messages[1].Content), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Outputs) != 1 || body.Outputs[0].Evidence != messages[1].Content || !strings.Contains(body.Outputs[0].Evidence, "EXACT FINDING") {
		t.Fatal("summarizer received clipped evidence")
	}
	for _, text := range []string{"12345", "2026-11-27", "https://example.com/report", "https://example.com/disagreement", "Summary of earlier web evidence"} {
		if !strings.Contains(got[1].Content, text) {
			t.Fatalf("lost evidence: %s", text)
		}
	}
	if got[2].Content != messages[2].Content || got[3].Content != messages[3].Content || got[0].ToolCalls[0].Arguments != messages[0].ToolCalls[0].Arguments {
		t.Fatal("touched fresh evidence, another tool or notes")
	}
	if !strings.Contains(messages[1].Content, "EXACT FINDING") {
		t.Fatal("mutated original output")
	}
	again := r.compactWebMessages(context.Background(), got, 7)
	if again[1].Content != got[1].Content || len(fake.Requests) != 1 {
		t.Fatal("summarized the same source twice")
	}
	entries := r.s.CostLog.(*costlog.InMemoryStore).Entries()
	if len(entries) != 1 || entries[0].Mode != "web_summary" || entries[0].ModelID != "gpt-6-luna" {
		t.Fatal("helper cost not recorded separately")
	}
}

func TestWebSummaryFailureKeepsOriginalsAndDoesNotRetry(t *testing.T) {
	for _, kind := range []string{"network", "malformed", "empty", "missing", "duplicate", "truncated", "too_long", "unavailable", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			r, messages, fake := summaryRun(t)
			switch kind {
			case "network":
				fake.Err = errors.New("upstream failed")
			case "malformed":
				fake.Responses[0].Text = "not JSON"
			case "empty":
				fake.Responses[0].Text = `{"summaries":[{"id":"old","summary":" "}]}`
			case "missing":
				fake.Responses[0].Text = `{"summaries":[]}`
			case "duplicate":
				fake.Responses[0].Text = `{"summaries":[{"id":"old","summary":"fact"},{"id":"old","summary":"fact"}]}`
			case "truncated":
				fake.Responses[0].FinishReason = "length"
			case "too_long":
				b, _ := json.Marshal(map[string]any{"summaries": []map[string]string{{"id": "old", "summary": messages[1].Content}}})
				fake.Responses[0].Text = string(b)
			case "unavailable":
				r.s.Summarizer.Client = nil
			}
			ctx := context.Background()
			if kind == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			for _, round := range []int{6, 7, 8} {
				got := r.compactWebMessages(ctx, messages, round)
				for i := range messages {
					if got[i].Content != messages[i].Content {
						t.Fatal("lost original evidence after failed summary")
					}
				}
			}
			if len(fake.Requests) > 1 {
				t.Fatal("repeated a failed paid helper call")
			}
		})
	}
}

func TestWebSummaryBatchesOlderOutputsAndKeepsOversizedOutput(t *testing.T) {
	r, messages, fake := summaryRun(t)
	r.webRecords["second"] = webRecord{round: 1, compact: "https://example.com/second"}
	r.webRecords["oversized"] = webRecord{round: 1, compact: "https://example.com/large"}
	messages = append(messages, provider.Message{Role: "tool", ToolCallID: "second", Content: strings.Repeat("other concrete findings ", 500)}, provider.Message{Role: "tool", ToolCallID: "oversized", Content: strings.Repeat("unabridged ", 6000)})
	fake.Responses[0].Text = `{"summaries":[{"id":"old","summary":"Total 12345. https://example.com/report"},{"id":"second","summary":"Additional concrete findings. https://example.com/second"}]}`
	got := r.compactWebMessages(context.Background(), messages, 6)
	if len(fake.Requests) != 1 || !strings.Contains(got[1].Content, "Total 12345") || !strings.Contains(got[4].Content, "Additional concrete findings") || got[5].Content != messages[5].Content {
		t.Fatal("did not batch complete eligible outputs or clipped oversized evidence")
	}
	var body struct {
		Outputs []evidenceSummaryInput `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(fake.Requests[0].Messages[1].Content), &body); err != nil || len(body.Outputs) != 2 {
		t.Fatal("expected one helper call for two complete outputs", err)
	}
}

func TestWebSummaryNormalModeKeepsSevenFollowingSteps(t *testing.T) {
	r, messages, fake := summaryRun(t)
	r.req.SearchMode = false
	messages[2].Content = strings.Repeat("recent evidence ", 3500)
	for round := 2; round < 9; round++ {
		if got := r.compactWebMessages(context.Background(), messages, round); got[1].Content != messages[1].Content || len(fake.Requests) != 0 {
			t.Fatal("normal mode summarized too soon")
		}
	}
	got := r.compactWebMessages(context.Background(), messages, 9)
	if len(fake.Requests) != 1 || !strings.Contains(got[1].Content, "Summary of earlier") || got[2].Content != messages[2].Content {
		t.Fatal("normal mode failed to summarize old evidence while retaining recent output")
	}
}

type webSummaryFunc func(context.Context, string, []provider.Message) (provider.GenerateResult, error)

func (f webSummaryFunc) Generate(ctx context.Context, model string, messages []provider.Message) (provider.GenerateResult, error) {
	return f(ctx, model, messages)
}
