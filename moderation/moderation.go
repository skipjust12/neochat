// Package moderation checks a user's request text against usage policy by
// calling a cheap model with the system prompt in
// prompts/moderation_system_prompt.md. This is Layer 1 ("input") of the
// two-layer design in README's "Moderation" section -- Layer 2 (checking
// generated output incrementally during streaming) needs a streaming
// response pipeline this repo doesn't have yet, so it isn't built here.
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
}

// New builds a Moderator from an already-loaded system prompt (see
// LoadSystemPrompt).
func New(client provider.Client, apiModelID, systemPrompt string) Moderator {
	return Moderator{Client: client, APIModelID: apiModelID, SystemPrompt: systemPrompt}
}

// Moderate sends text to the moderation model and parses its JSON reply.
// Mirrors classifier.Classify's fence-stripping defense against a model
// wrapping its reply in a ```json fence despite instructions not to.
func (m Moderator) Moderate(ctx context.Context, text string) (Result, error) {
	result, err := m.Client.Generate(ctx, m.APIModelID, []provider.Message{
		{Role: "system", Content: m.SystemPrompt},
		{Role: "user", Content: text},
	})
	if err != nil {
		return Result{}, fmt.Errorf("moderation: generate: %w", err)
	}

	var out Result
	if err := json.Unmarshal([]byte(stripFences(result.Text)), &out); err != nil {
		return Result{}, fmt.Errorf("moderation: parse model reply as JSON: %w (reply=%q)", err, result.Text)
	}
	return out, nil
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
