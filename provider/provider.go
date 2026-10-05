// Package provider calls model vendors. Today every catalog model goes
// through Polza AI (PolzaClient); Client is the seam a hybrid-sourcing
// setup (an aggregator for most models, direct contracts for a few) sits
// behind, so adding a vendor later means writing a new Client
// implementation, not touching callers.
package provider

import (
	"context"
	"encoding/json"
)

// Message is one turn in a chat-style completion request.
//
// Content is always the turn's plain text. Parts, when set, is the full
// multimodal body (text, images, documents) and replaces Content on the
// wire; Content then stays the text-only view used for estimates, logs and
// summaries.
type Message struct {
	Role    string // "system" | "user" | "assistant" | "tool"
	Content string
	Parts   []Part

	// ToolCalls are the function calls an assistant turn asked for (see
	// WithFunctionTools); ToolCallID ties a "tool" turn -- a function's
	// result, in Content -- to one of them.
	ToolCalls  []ToolCall
	ToolCallID string

	// Reasoning and ReasoningDetails are an assistant turn's reasoning as
	// the vendor returned it, sent back with that turn's tool calls: some
	// models (Claude with thinking, Gemini 3) need their reasoning to carry
	// on after a tool result.
	Reasoning        string
	ReasoningDetails json.RawMessage
}

// ToolCall is one function call requested by the model. Arguments is the
// raw JSON object the model produced.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Part kinds.
const (
	PartText  = "text"
	PartImage = "image"
	PartFile  = "file"
	PartVideo = "video"
)

// Part is one piece of a multimodal message. Text parts carry Text; image,
// file and video parts carry the raw bytes plus their MIME type and file
// name.
type Part struct {
	Type string
	Text string
	MIME string
	Name string
	Data []byte
}

// GenerateResult is a completed generation: the text plus the real token
// usage the vendor reported, needed for router.ComputeCostUSD /
// limits.RecordThinkingMaxSpend -- estimates are only good enough for
// pre-flight checks, billing runs on this.
type GenerateResult struct {
	Text         string
	InputTokens  int
	OutputTokens int

	// CachedTokens and CacheWriteTokens are the part of InputTokens read
	// from and written to the vendor's prompt cache (see cache.go).
	// CostRUB is what Polza charged for the call, cache discounts and
	// server tools included; 0 when it didn't say.
	CachedTokens     int
	CacheWriteTokens int
	CostRUB          float64

	// ToolCalls are the functions the model asked to call (FinishReason
	// "tool_calls"); the caller runs them and continues the conversation.
	// Reasoning/ReasoningDetails come back with them -- see Message.
	ToolCalls        []ToolCall
	FinishReason     string
	Reasoning        string
	ReasoningDetails json.RawMessage

	// Citations are url_citation annotations (web plugin results);
	// WebSearches is the number of searches the vendor billed for.
	Citations   []WebResult
	WebSearches int
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
// per stream: zero or more chunks with Delta (or Reasoning) set, followed by either one
// chunk with Done set and Final populated (success) or one chunk with Err
// set (failure) -- the channel is always closed right after that terminal
// chunk, so a range loop naturally ends there.
type StreamChunk struct {
	Delta string
	// Reasoning is a piece of the model's thinking text (reasoning models
	// that share it), sent before and between Delta chunks. It is never
	// part of the answer.
	Reasoning string
	Err       error

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
