package costlog

import (
	"context"
	"sync"

	"neochat/router"
)

// InMemoryStore is a process-local Store backed by a plain slice, safe for
// concurrent use. Nothing persists across a restart and there is no
// cross-instance sharing -- exactly as much "database" as makes sense
// before there is a server to give one a job to do.
type InMemoryStore struct {
	mu      sync.Mutex
	entries []router.CostLogEntry
}

// NewInMemoryStore returns an empty, ready-to-use store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{}
}

func (s *InMemoryStore) Record(_ context.Context, entry router.CostLogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

// Entries returns a copy of every entry recorded so far, oldest first.
// Not part of the Store interface -- a real store would expose this
// through a query, not an in-memory snapshot -- but it's useful for tests
// and for inspecting what's accumulated during local development.
func (s *InMemoryStore) Entries() []router.CostLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]router.CostLogEntry, len(s.entries))
	copy(out, s.entries)
	return out
}
