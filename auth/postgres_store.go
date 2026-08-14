package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PostgresStore is a Store backed by the api_keys table
// (db/migrations/0004_api_keys.sql). See Authenticate/IssueKey.
type PostgresStore struct {
	db *sql.DB
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
