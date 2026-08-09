// Package provider calls model vendors directly (not through OpenRouter).
// This is the "Provider abstraction layer" item from README's pre-launch
// checklist: Client is the seam a hybrid-sourcing setup (OpenRouter for
// most models, direct contracts for a few) sits behind, so adding a
// vendor later means writing a new Client implementation, not touching
// callers.
package provider

import "context"

// Message is one turn in a chat-style completion request.
type Message struct {
	Role    string // "system" | "user" | "assistant"
	Content string
}

// GenerateResult is a completed generation: the text plus the real token
// usage the vendor reported, needed for router.ComputeCostUSD /
// limits.RecordThinkingMaxSpend -- estimates are only good enough for
// pre-flight checks, billing runs on this.
type GenerateResult struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// Client generates a completion from one vendor's API. apiModelID is the
// vendor's own model identifier, which is not necessarily the same string
// as router.Model.ID (the catalog's internal name) -- see
// router.Model.ResolveAPIModelID.
type Client interface {
	Generate(ctx context.Context, apiModelID string, messages []Message) (GenerateResult, error)
}
