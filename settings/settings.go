// Package settings keeps each user's preferences (tone, custom
// instructions, default model, theme, ...) on the server, so every device
// they sign in on starts from the same ones. The server treats them as an
// opaque JSON object the web UI defines; API keys are never part of it.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Store keeps one settings object per user.
type Store interface {
	// Get returns the user's settings, or nil when none were saved yet.
	Get(ctx context.Context, userID string) (json.RawMessage, error)
	// Put replaces them.
	Put(ctx context.Context, userID string, value json.RawMessage) error
}

// InMemoryStore is a Store for tests.
type InMemoryStore struct {
	mu   sync.Mutex
	data map[string]json.RawMessage
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{data: map[string]json.RawMessage{}}
}

func (s *InMemoryStore) Get(_ context.Context, userID string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(json.RawMessage(nil), s.data[userID]...), nil
}

func (s *InMemoryStore) Put(_ context.Context, userID string, value json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[userID] = append(json.RawMessage(nil), value...)
	return nil
}

// PostgresStore is a Store over user_settings
// (db/migrations/0012_user_settings.sql).
type PostgresStore struct{ db *sql.DB }

func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{db: db} }

func (s *PostgresStore) Get(ctx context.Context, userID string) (json.RawMessage, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx, `SELECT settings FROM user_settings WHERE user_id = $1`, userID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return value, err
}

func (s *PostgresStore) Put(ctx context.Context, userID string, value json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_settings (user_id, settings, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET settings = EXCLUDED.settings, updated_at = EXCLUDED.updated_at
	`, userID, []byte(value), time.Now())
	return err
}
