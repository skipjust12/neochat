package idempotency

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
)

type InMemoryStore struct {
	mu      sync.Mutex
	entries map[compositeKey]entry
}
type compositeKey struct{ userID, key string }
type entry struct {
	owner    string
	response []byte
}

func newOwner() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func NewInMemoryStore() *InMemoryStore { return &InMemoryStore{entries: make(map[compositeKey]entry)} }

func (s *InMemoryStore) Reserve(ctx context.Context, userID, key string) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ck := compositeKey{userID, key}
	if e, ok := s.entries[ck]; ok {
		if e.owner != "" {
			return Record{}, false, ErrInFlight
		}
		return Record{Response: append([]byte(nil), e.response...)}, true, nil
	}
	owner, err := newOwner()
	if err != nil {
		return Record{}, false, err
	}
	s.entries[ck] = entry{owner: owner}
	return Record{Owner: owner}, false, nil
}

func (s *InMemoryStore) Complete(ctx context.Context, userID, key string, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ck := compositeKey{userID, key}
	e, ok := s.entries[ck]
	if !ok || e.owner == "" || e.owner != record.Owner {
		return ErrLeaseLost
	}
	s.entries[ck] = entry{response: append([]byte(nil), record.Response...)}
	return nil
}

func (s *InMemoryStore) Release(ctx context.Context, userID, key, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ck := compositeKey{userID, key}
	e, ok := s.entries[ck]
	if !ok || e.owner == "" || e.owner != owner {
		return ErrLeaseLost
	}
	delete(s.entries, ck)
	return nil
}
