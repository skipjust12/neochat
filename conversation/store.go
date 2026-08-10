package conversation

import (
	"context"
	"sync"
)

// Store is the seam between this package and wherever conversation
// history actually lives. InMemoryStore (below) is a deliberately
// throwaway stand-in -- the same role limits.SpendStore/InMemorySpendStore
// and moderation.BlockLog/InMemoryBlockLog play for their own data.
//
// Swap-in procedure once a real database exists: implement this
// interface against Postgres (a message-per-row table keyed on
// (user_id, conversation_id), ordered by an inserted-at column, is the
// direct translation of InMemoryStore's scheme), point callers at the
// new implementation instead of NewInMemoryStore, and delete
// InMemoryStore. Nothing in server/ depends on which implementation is
// in use, only on this interface.
type Store interface {
	// Append adds one message to (userID, conversationID)'s history, in
	// order.
	Append(ctx context.Context, userID, conversationID string, msg Message) error

	// History returns every message recorded for (userID, conversationID),
	// oldest first. An unknown conversation returns an empty slice, not
	// an error -- a brand new conversation simply has no history yet.
	History(ctx context.Context, userID, conversationID string) ([]Message, error)
}

// InMemoryStore is a process-local Store backed by a plain map, safe for
// concurrent use. Nothing persists across a restart and there is no
// cross-instance sharing -- exactly as much "database" as makes sense
// before there is a server to give one a job to do.
//
// Keys are (userID, conversationID) pairs, not conversationID alone, so
// one user can never read or extend another user's history even if
// conversation IDs were ever guessed or collided -- the same scoping
// limits.SpendStore already applies to spend, applied here to history.
type InMemoryStore struct {
	mu      sync.Mutex
	history map[conversationKey][]Message
}

type conversationKey struct {
	userID         string
	conversationID string
}

// NewInMemoryStore returns an empty, ready-to-use store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{history: make(map[conversationKey][]Message)}
}

func (s *InMemoryStore) Append(_ context.Context, userID, conversationID string, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID: userID, conversationID: conversationID}
	s.history[key] = append(s.history[key], msg)
	return nil
}

func (s *InMemoryStore) History(_ context.Context, userID, conversationID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID: userID, conversationID: conversationID}
	out := make([]Message, len(s.history[key]))
	copy(out, s.history[key])
	return out, nil
}
