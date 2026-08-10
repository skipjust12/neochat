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
	Role    Role   `json:"role"`
	Content string `json:"content"`

	// ModelID is which catalog model produced this message -- empty for
	// user turns, the router's selected model ID for assistant turns.
	// This is exactly the "which model answered a given message" example
	// the checklist item calls out as needing to be there from day one.
	ModelID   string    `json:"model_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
