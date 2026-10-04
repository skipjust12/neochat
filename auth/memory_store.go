package auth

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// InMemoryStore is a process-local Store and SessionStore backed by plain
// maps, safe for concurrent use -- this package's counterpart to
// idempotency.InMemoryStore/moderation.InMemoryBlockLog, for tests that
// don't need a real Postgres. Never wired into cmd/server: an issued key
// must survive a restart, so the real deployment always uses
// PostgresStore.
type InMemoryStore struct {
	mu       sync.Mutex
	keys     map[string]Identity        // keyed by hashToken(token), never the raw token
	sessions map[string]inMemorySession // keyed by hashToken(session token)
	now      func() time.Time           // nil: time.Now
}

type inMemorySession struct {
	keyHash   string
	createdAt time.Time
	expiresAt time.Time
}

// NewInMemoryStore returns an empty, ready-to-use store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{keys: make(map[string]Identity), sessions: make(map[string]inMemorySession)}
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

// RevokeKey deletes an API key, the way an operator deletes its api_keys
// row; the sessions it signed in stop working with it.
func (s *InMemoryStore) RevokeKey(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, hashToken(token))
}

func (s *InMemoryStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *InMemoryStore) CreateSession(_ context.Context, apiKey string) (string, Session, error) {
	if apiKey == "" {
		return "", Session{}, ErrInvalidToken
	}
	token, err := generateSessionToken()
	if err != nil {
		return "", Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keyHash := hashToken(apiKey)
	identity, ok := s.keys[keyHash]
	if !ok {
		return "", Session{}, ErrInvalidToken
	}
	now := s.clock()
	session := Session{Identity: identity, ExpiresAt: now.Add(SessionTTL)}
	s.sessions[hashToken(token)] = inMemorySession{keyHash: keyHash, createdAt: now, expiresAt: session.ExpiresAt}

	var mine []string
	for hash, stored := range s.sessions {
		if stored.keyHash == keyHash {
			mine = append(mine, hash)
		}
	}
	sort.Slice(mine, func(i, j int) bool {
		a, b := s.sessions[mine[i]], s.sessions[mine[j]]
		if !a.createdAt.Equal(b.createdAt) {
			return a.createdAt.After(b.createdAt)
		}
		return mine[i] < mine[j]
	})
	for _, hash := range mine[min(len(mine), MaxSessionsPerKey):] {
		delete(s.sessions, hash)
	}
	return token, session, nil
}

func (s *InMemoryStore) ResumeSession(_ context.Context, token string) (Session, error) {
	if !validSessionToken(token) {
		return Session{}, ErrInvalidToken
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := hashToken(token)
	stored, ok := s.sessions[hash]
	now := s.clock()
	if !ok || !stored.expiresAt.After(now) {
		return Session{}, ErrInvalidToken
	}
	identity, ok := s.keys[stored.keyHash]
	if !ok {
		return Session{}, ErrInvalidToken
	}
	session := Session{Identity: identity, ExpiresAt: stored.expiresAt}
	if sessionDue(stored.expiresAt, now) {
		stored.expiresAt = now.Add(SessionTTL)
		s.sessions[hash] = stored
		session.ExpiresAt, session.Extended = stored.expiresAt, true
	}
	return session, nil
}

func (s *InMemoryStore) EndSession(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, hashToken(token))
	return nil
}

func (s *InMemoryStore) SweepSessions(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted int64
	for hash, stored := range s.sessions {
		if _, ok := s.keys[stored.keyHash]; !ok || !stored.expiresAt.After(now) {
			delete(s.sessions, hash)
			deleted++
		}
	}
	return deleted, nil
}
