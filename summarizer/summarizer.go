// Package summarizer compresses the older part of a long conversation
// into a short rolling text summary by calling a cheap model with the
// system prompt in prompts/summarizer_system_prompt.md. It exists to
// close the "Context window mismatch" gap documented in README.md: once
// a conversation's stored history grows past what any catalog model can
// accept, server.prepare needs something smaller than the full history to
// send instead of simply failing to route.
package summarizer

import (
	"context"
	"fmt"
	"strings"

	"neochat/conversation"
	"neochat/provider"
)

// Summarizer calls one provider.Client/model pair with the summarizer
// system prompt to fold a batch of messages into an updated summary.
type Summarizer struct {
	Client            provider.Client
	APIModelID        string
	SystemPrompt      string
	CostInputPerMTok  float64
	CostOutputPerMTok float64
}

// New builds a Summarizer from an already-loaded system prompt (see
// LoadSystemPrompt).
func New(client provider.Client, apiModelID, systemPrompt string) Summarizer {
	return Summarizer{Client: client, APIModelID: apiModelID, SystemPrompt: systemPrompt}
}

// Summarize folds previousSummary (empty for a conversation's first
// summarization pass) and newMessages into an updated summary text.
// newMessages must be non-empty -- callers only invoke this when there is
// actually something new to fold in (see server.maybeSummarize).
func (s Summarizer) Summarize(ctx context.Context, previousSummary string, newMessages []conversation.Message) (string, error) {
	if len(newMessages) == 0 {
		return "", fmt.Errorf("summarizer: newMessages is empty, nothing to summarize")
	}

	var b strings.Builder
	if previousSummary != "" {
		b.WriteString("Existing summary of the conversation so far:\n")
		b.WriteString(previousSummary)
		b.WriteString("\n\n")
	}
	b.WriteString("New messages to fold into the summary:\n")
	for _, m := range newMessages {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}

	result, err := s.Client.Generate(ctx, s.APIModelID, []provider.Message{
		{Role: "system", Content: s.SystemPrompt},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return "", fmt.Errorf("summarizer: generate: %w", err)
	}

	text := strings.TrimSpace(result.Text)
	if text == "" {
		return "", fmt.Errorf("summarizer: model returned empty summary")
	}
	return text, nil
}
