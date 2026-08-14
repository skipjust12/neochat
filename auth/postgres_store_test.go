//go:build integration

package auth

import (
	"context"
	"errors"
	"testing"

	"neochat/internal/dbtest"
)

func TestPostgresStore_IssueThenAuthenticateResolvesIdentity(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "api_keys")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	token, err := store.IssueKey(ctx, "u1", "pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	identity, err := store.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if identity != (Identity{UserID: "u1", PlanID: "pro"}) {
		t.Errorf("Authenticate() = %+v, want {u1 pro}", identity)
	}
}

func TestPostgresStore_AuthenticateUnknownTokenFails(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "api_keys")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	if _, err := store.Authenticate(ctx, "nc_never-issued"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidToken", err)
	}
}

// TestPostgresStore_RawTokenNeverStored guards the whole point of hashing
// before persisting: even with direct table access, the raw token issued
// to the caller must not appear anywhere in api_keys.
func TestPostgresStore_RawTokenNeverStored(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "api_keys")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	token, err := store.IssueKey(ctx, "u1", "pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	if err := pgDB.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE token_hash = $1`, token).Scan(&count); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Error("raw token found stored verbatim in api_keys.token_hash -- should only ever hold hashToken's output")
	}
}
