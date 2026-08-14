package auth

import (
	"context"
	"fmt"
	"sync"
)

// InMemoryStore is a process-local Store backed by a plain map, safe for
// concurrent use -- this package's counterpart to
// idempotency.InMemoryStore/moderation.InMemoryBlockLog, for tests that
// don't need a real Postgres. Never wired into cmd/server: an issued key
// must survive a restart, so the real deployment always uses
// PostgresStore.
type InMemoryStore struct {
	mu   sync.Mutex
	keys map[string]Identity // keyed by hashToken(token), never the raw token
}

// NewInMemoryStore returns an empty, ready-to-use store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{keys: make(map[string]Identity)}
}

func (s *InMemoryStore) Authenticate(_ context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, ErrInvalidToken
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identity, ok := s.keys[hashToken(token)]
	if !ok {
		return Identity{}, ErrInvalidToken
	}
	return identity, nil
}

func (s *InMemoryStore) IssueKey(_ context.Context, userID, planID string) (string, error) {
	if userID == "" || planID == "" {
		return "", fmt.Errorf("auth: issue key: user_id and plan_id are required")
	}
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[hashToken(token)] = Identity{UserID: userID, PlanID: planID}
	return token, nil
}
