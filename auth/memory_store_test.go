package auth

import (
	"context"
	"errors"
	"testing"
)

func TestInMemoryStore_IssueThenAuthenticateResolvesIdentity(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	token, err := store.IssueKey(ctx, "u1", "pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token == "" {
		t.Fatal("IssueKey returned an empty token")
	}

	identity, err := store.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if identity != (Identity{UserID: "u1", PlanID: "pro"}) {
		t.Errorf("Authenticate() = %+v, want {u1 pro}", identity)
	}
}

func TestInMemoryStore_AuthenticateUnknownTokenFails(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	if _, err := store.Authenticate(ctx, "nc_never-issued"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidToken", err)
	}
}

func TestInMemoryStore_AuthenticateEmptyTokenFails(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	if _, err := store.Authenticate(ctx, ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidToken", err)
	}
}

// TestInMemoryStore_EachIssuedTokenIsUnique guards against a broken
// generateToken silently handing out colliding (or worse, predictable)
// credentials -- two keys minted back to back for the same user must
// still be two distinct, independently valid tokens.
func TestInMemoryStore_EachIssuedTokenIsUnique(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	a, err := store.IssueKey(ctx, "u1", "pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := store.IssueKey(ctx, "u1", "pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == b {
		t.Fatalf("IssueKey returned the same token twice: %q", a)
	}

	if _, err := store.Authenticate(ctx, a); err != nil {
		t.Errorf("first token no longer authenticates: %v", err)
	}
	if _, err := store.Authenticate(ctx, b); err != nil {
		t.Errorf("second token doesn't authenticate: %v", err)
	}
}

func TestInMemoryStore_IssueKeyRequiresUserIDAndPlanID(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	if _, err := store.IssueKey(ctx, "", "pro"); err == nil {
		t.Error("IssueKey with empty user_id should error")
	}
	if _, err := store.IssueKey(ctx, "u1", ""); err == nil {
		t.Error("IssueKey with empty plan_id should error")
	}
}

// TestInMemoryStore_ImplementsStore is a compile-time-ish check (caught
// at test-build time rather than in every caller) that InMemoryStore
// hasn't drifted from the Store interface it's meant to mirror
// PostgresStore against.
func TestInMemoryStore_ImplementsStore(t *testing.T) {
	var _ Store = NewInMemoryStore()
}
