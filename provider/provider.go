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

// StreamChunk is one piece of an in-progress streamed generation, sent on
// the channel GenerateStream returns. Exactly one of two shapes appears
// per stream: zero or more chunks with Delta set, followed by either one
// chunk with Done set and Final populated (success) or one chunk with Err
// set (failure) -- the channel is always closed right after that terminal
// chunk, so a range loop naturally ends there.
type StreamChunk struct {
	Delta string
	Err   error

	Done  bool
	Final GenerateResult // populated only when Done is true
}

// StreamingClient is a Client that can additionally stream a generation as
// the vendor produces it, instead of the caller waiting for the whole
// response. Not every Client needs this: classifier/moderation only ever
// consume a full parsed JSON reply, so streaming would buy them nothing --
// this is a separate interface rather than a new Client method so those
// callers, and any Client implementation that doesn't need it, aren't
// forced to grow a no-op version.
type StreamingClient interface {
	GenerateStream(ctx context.Context, apiModelID string, messages []Message) (<-chan StreamChunk, error)
}
