// Package moderation checks a user's request text against usage policy by
// calling a cheap model with the system prompt in
// prompts/moderation_system_prompt.md. This is Layer 1 ("input") of the
// two-layer design in README's "Moderation" section -- Layer 2 (checking
// generated output incrementally during streaming) is a deliberate
// product decision not to build, not a gap waiting on a streaming
// pipeline (POST /chat/stream exists): every catalog model already
// carries its own vendor-side safety tuning, so Layer 2 would only ever
// catch a jailbreak that both slipped past this layer's input check and
// got a frontier model to comply anyway -- a narrow case not worth a
// second moderation-model call on every request. See README "Moderation".
package moderation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"neochat/provider"
)

// Result is the moderation model's verdict on one piece of text.
type Result struct {
	Flagged    bool     `json:"flagged"`
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
}

// Moderator calls one provider.Client/model pair with the moderation
// system prompt and parses its reply into a Result.
type Moderator struct {
	Client       provider.Client
	APIModelID   string
	SystemPrompt string

	// CostInputPerMTok/CostOutputPerMTok price Moderate's real token usage
	// for cost_log (see Moderate's returned *provider.GenerateResult and
	// README pre-launch checklist item 7). Zero value logs every call at
	// $0 rather than failing -- fine for tests, but a real deployment
	// should set these to match whatever model APIModelID actually is
	// (cmd/server/main.go's MODERATION_COST_*_PER_MTOK), since nothing
	// here cross-checks the two stay in sync.
	CostInputPerMTok  float64
	CostOutputPerMTok float64
}

// New builds a Moderator from an already-loaded system prompt (see
// LoadSystemPrompt).
func New(client provider.Client, apiModelID, systemPrompt string) Moderator {
	return Moderator{Client: client, APIModelID: apiModelID, SystemPrompt: systemPrompt}
}

// Moderate sends text to the moderation model and parses its JSON reply.
// Mirrors classifier.Classify's fence-stripping defense against a model
// wrapping its reply in a ```json fence despite instructions not to.
//
// The returned *provider.GenerateResult is real token usage for cost
// logging, nil only when Client.Generate itself never completed (no
// tokens were ever billed); it's still populated on a JSON-parse failure
// below, since that failure happens after a real, billed vendor call.
func (m Moderator) Moderate(ctx context.Context, text string) (Result, *provider.GenerateResult, error) {
	result, err := m.Client.Generate(ctx, m.APIModelID, []provider.Message{
		{Role: "system", Content: m.SystemPrompt},
		{Role: "user", Content: text},
	})
	if err != nil {
		return Result{}, nil, fmt.Errorf("moderation: generate: %w", err)
	}

	var out Result
	if err := json.Unmarshal([]byte(stripFences(result.Text)), &out); err != nil {
		return Result{}, &result, fmt.Errorf("moderation: parse model reply as JSON: %w (reply=%q)", err, result.Text)
	}
	return out, &result, nil
}

// stripFences removes a wrapping ```json ... ``` or ``` ... ``` fence, if
// present, and trims surrounding whitespace. Returns the input unchanged
// if there's no fence to strip.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
