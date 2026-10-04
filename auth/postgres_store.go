package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PostgresStore is a Store backed by the api_keys table
// (db/migrations/0004_api_keys.sql), and the SessionStore over
// web_sessions (db/migrations/0010_web_sessions.sql). See
// Authenticate/IssueKey and session.go.
type PostgresStore struct {
	db  *sql.DB
	now func() time.Time // nil: time.Now
}

// NewPostgresStore returns a Store backed by db. Callers are responsible
// for migrating db first (see db.Connect, which does this automatically).
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Authenticate(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, ErrInvalidToken
	}
	var identity Identity
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, plan_id FROM api_keys WHERE token_hash = $1
	`, hashToken(token)).Scan(&identity.UserID, &identity.PlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrInvalidToken
	}
	if err != nil {
		return Identity{}, fmt.Errorf("auth: authenticate: %w", err)
	}
	return identity, nil
}

// IssueKey mints a fresh API key for (userID, planID) and stores only its
// hash (see hashToken) -- the returned raw token is the one and only copy
// anyone will ever see. Only cmd/issuekey calls this today; see the
// package doc comment for why server.Server itself never does.
func (s *PostgresStore) IssueKey(ctx context.Context, userID, planID string) (string, error) {
	if userID == "" || planID == "" {
		return "", fmt.Errorf("auth: issue key: user_id and plan_id are required")
	}
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (token_hash, user_id, plan_id, created_at)
		VALUES ($1, $2, $3, now())
	`, hashToken(token), userID, planID); err != nil {
		return "", fmt.Errorf("auth: issue key: %w", err)
	}
	return token, nil
}

func (s *PostgresStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *PostgresStore) CreateSession(ctx context.Context, apiKey string) (string, Session, error) {
	if apiKey == "" {
		return "", Session{}, ErrInvalidToken
	}
	keyHash := hashToken(apiKey)
	var session Session
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, plan_id FROM api_keys WHERE token_hash = $1
	`, keyHash).Scan(&session.UserID, &session.PlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Session{}, ErrInvalidToken
	}
	if err != nil {
		return "", Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	token, err := generateSessionToken()
	if err != nil {
		return "", Session{}, err
	}
	now := s.clock()
	session.ExpiresAt = now.Add(SessionTTL)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO web_sessions (token_hash, key_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4)
	`, hashToken(token), keyHash, now, session.ExpiresAt); err != nil {
		return "", Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	// Keep the key's newest MaxSessionsPerKey sessions.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM web_sessions WHERE token_hash IN (
			SELECT token_hash FROM web_sessions WHERE key_hash = $1
			ORDER BY created_at DESC, token_hash OFFSET $2
		)
	`, keyHash, MaxSessionsPerKey); err != nil {
		return "", Session{}, fmt.Errorf("auth: create session: trim old sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	return token, session, nil
}

func (s *PostgresStore) ResumeSession(ctx context.Context, token string) (Session, error) {
	if !validSessionToken(token) {
		return Session{}, ErrInvalidToken
	}
	hash := hashToken(token)
	now := s.clock()
	var session Session
	err := s.db.QueryRowContext(ctx, `
		SELECT k.user_id, k.plan_id, ws.expires_at
		FROM web_sessions ws JOIN api_keys k ON k.token_hash = ws.key_hash
		WHERE ws.token_hash = $1 AND ws.expires_at > $2
	`, hash, now).Scan(&session.UserID, &session.PlanID, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrInvalidToken
	}
	if err != nil {
		return Session{}, fmt.Errorf("auth: resume session: %w", err)
	}
	if sessionDue(session.ExpiresAt, now) {
		expiresAt := now.Add(SessionTTL)
		if _, err := s.db.ExecContext(ctx, `
			UPDATE web_sessions SET expires_at = $2 WHERE token_hash = $1 AND expires_at < $2
		`, hash, expiresAt); err != nil {
			return Session{}, fmt.Errorf("auth: extend session: %w", err)
		}
		session.ExpiresAt, session.Extended = expiresAt, true
	}
	return session, nil
}

func (s *PostgresStore) EndSession(ctx context.Context, token string) error {
	if !validSessionToken(token) {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash = $1`, hashToken(token)); err != nil {
		return fmt.Errorf("auth: end session: %w", err)
	}
	return nil
}

func (s *PostgresStore) SweepSessions(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM web_sessions ws
		WHERE ws.expires_at <= $1
		   OR NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.token_hash = ws.key_hash)
	`, now)
	if err != nil {
		return 0, fmt.Errorf("auth: sweep sessions: %w", err)
	}
	return result.RowsAffected()
}
