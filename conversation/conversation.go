// Package conversation stores per-conversation chat history so a /chat
// request can send a model more than just the single latest message --
// see README's "Conversation storage schema built for migration from day
// one" pre-launch checklist item.
package conversation

import "time"

// Role identifies who sent a stored message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one stored turn in a conversation. Deliberately just these
// fields for now -- the checklist item asks for a schema that can grow
// (e.g. separating reasoning blocks from the final answer, or
// per-message metadata like "thought for N seconds") without breaking
// changes. A Go struct with JSON tags already satisfies that: new
// optional fields can be added later without touching what's here, as
// long as they default sanely (zero value) for older stored rows.
type Message struct {
	ID        int64  `json:"id,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Role      Role   `json:"role"`
	Content   string `json:"content"`

	// ModelID is which catalog model produced this message -- empty for
	// user turns, the router's selected model ID for assistant turns.
	// This is exactly the "which model answered a given message" example
	// the checklist item calls out as needing to be there from day one.
	ModelID   string    `json:"model_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// IsSummary marks a message as machine-generated summary content
	// rather than a real turn someone typed/received -- not read anywhere
	// yet, reserved for when summarizer-produced text needs to be told
	// apart from ordinary history (e.g. excluding it from re-summarization
	// input, or flagging it in a transcript export).
	IsSummary bool              `json:"is_summary,omitempty"`
	Versions  []ResponseVersion `json:"versions,omitempty"`
}

const MaxRegenerationAttempts = 10

type ResponseVersion struct {
	Content   string    `json:"content"`
	ModelID   string    `json:"model_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Overview is the compact representation used by conversation lists.
// The title is derived from the first user message, so saving a chat does
// not require a second metadata write or a separate naming flow.
type Overview struct {
	ID        string    `json:"conversation_id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Summary is the rolling, machine-generated compression of a
// conversation's older messages, kept alongside the full History so a
// long-running conversation can stay within a model's context window
// without deleting anything from storage. See Store.GetSummary/SetSummary.
type Summary struct {
	// Text is the current summary content, folding in every message up to
	// CoversThrough.
	Text string `json:"text"`

	// CoversThrough is how many of the oldest messages in History (by
	// index, 0 meaning none) are already represented in Text. The next
	// summarization pass only needs to fold in History[CoversThrough:],
	// not the whole conversation again.
	CoversThrough int `json:"covers_through"`

	UpdatedAt time.Time `json:"updated_at"`
}

// Page is chronological, with a cursor for loading older messages.
type Page struct {
	Messages   []Message `json:"messages"`
	NextCursor int64     `json:"next_cursor,omitempty"`
}
