package idempotency

import (
	"context"
	"sync"
)

// InMemoryStore is a process-local Store backed by a plain map, safe for
// concurrent use. Nothing persists across a restart and there is no
// cross-instance sharing -- exactly as much "database" as makes sense
// before there is a server to give one a job to do (see Store's doc
// comment for the Redis swap-in procedure).
//
// Keys are (userID, key) pairs, not the idempotency key alone, so one
// user's key can never collide with another's -- the same scoping
// limits.SpendStore/conversation.Store already apply to their own data,
// applied here to idempotency records.
type InMemoryStore struct {
	mu      sync.Mutex
	entries map[compositeKey]entry
}

type compositeKey struct {
	userID string
	key    string
}

type entry struct {
	inFlight bool
	response []byte
}

// NewInMemoryStore returns an empty, ready-to-use store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{entries: make(map[compositeKey]entry)}
}

func (s *InMemoryStore) Reserve(_ context.Context, userID, key string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ck := compositeKey{userID: userID, key: key}
	e, ok := s.entries[ck]
	if !ok {
		s.entries[ck] = entry{inFlight: true}
		return Record{}, false, nil
	}
	if e.inFlight {
		return Record{}, false, ErrInFlight
	}
	return Record{Response: e.response}, true, nil
}

func (s *InMemoryStore) Complete(_ context.Context, userID, key string, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[compositeKey{userID: userID, key: key}] = entry{response: record.Response}
	return nil
}

func (s *InMemoryStore) Release(_ context.Context, userID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.entries, compositeKey{userID: userID, key: key})
	return nil
}
