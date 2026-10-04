package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// sessionStoreUnderTest is what testSessionStore needs besides the store:
// a way to move its clock and to delete an API key the way an operator
// would.
type sessionStoreUnderTest struct {
	store interface {
		Store
		SessionStore
	}
	setNow    func(time.Time)
	revokeKey func(token string)
}

// testSessionStore is the behaviour every SessionStore must have; the
// in-memory and Postgres stores both run it.
func testSessionStore(t *testing.T, s sessionStoreUnderTest) {
	ctx := context.Background()
	store := s.store
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := start
	s.setNow(now)
	advance := func(d time.Duration) {
		now = now.Add(d)
		s.setNow(now)
	}

	key, err := store.IssueKey(ctx, "u1", "pro")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateSession(ctx, "nc_not-a-key"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("CreateSession(unknown key) error = %v, want ErrInvalidToken", err)
	}
	if _, _, err := store.CreateSession(ctx, ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("CreateSession(\"\") error = %v, want ErrInvalidToken", err)
	}

	token, session, err := store.CreateSession(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "ncs_") || token == key {
		t.Fatalf("session token = %q", token)
	}
	if session.Identity != (Identity{UserID: "u1", PlanID: "pro"}) || !session.ExpiresAt.Equal(start.Add(SessionTTL)) {
		t.Fatalf("CreateSession() session = %+v", session)
	}
	// A session token is not an API key, and an API key is not a session.
	if _, err := store.Authenticate(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate(session token) error = %v, want ErrInvalidToken", err)
	}
	if _, err := store.ResumeSession(ctx, key); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("ResumeSession(API key) error = %v, want ErrInvalidToken", err)
	}
	for _, bad := range []string{"", "ncs_", "ncs_never-issued"} {
		if _, err := store.ResumeSession(ctx, bad); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("ResumeSession(%q) error = %v, want ErrInvalidToken", bad, err)
		}
	}

	// Used within a day: same expiry, nothing to re-send.
	advance(time.Hour)
	resumed, err := store.ResumeSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Identity != session.Identity || resumed.Extended || !resumed.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("ResumeSession() after an hour = %+v", resumed)
	}
	// Used a day and more later: the expiry slides out to SessionTTL from now.
	advance(sessionExtendEvery)
	resumed, err = store.ResumeSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Extended || !resumed.ExpiresAt.Equal(now.Add(SessionTTL)) {
		t.Fatalf("ResumeSession() after a day = %+v, want extended to %v", resumed, now.Add(SessionTTL))
	}
	// So a session used every few weeks never expires...
	for range 3 {
		advance(SessionTTL - time.Hour)
		if _, err := store.ResumeSession(ctx, token); err != nil {
			t.Fatalf("session in regular use expired: %v", err)
		}
	}
	// ...and one left alone for SessionTTL does.
	advance(SessionTTL)
	if _, err := store.ResumeSession(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("ResumeSession() after SessionTTL idle error = %v, want ErrInvalidToken", err)
	}

	// Logging out ends that session only; twice is fine.
	first, _, err := store.CreateSession(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.CreateSession(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(ctx, first); err != nil {
		t.Fatalf("second EndSession: %v", err)
	}
	if _, err := store.ResumeSession(ctx, first); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("ResumeSession(ended) error = %v, want ErrInvalidToken", err)
	}
	if _, err := store.ResumeSession(ctx, second); err != nil {
		t.Fatalf("ending one session ended another: %v", err)
	}

	// Sweep: the expired session (token) goes, the live one stays.
	deleted, err := store.SweepSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("SweepSessions() deleted %d, want 1 (the expired session)", deleted)
	}
	if _, err := store.ResumeSession(ctx, second); err != nil {
		t.Fatalf("sweep deleted a live session: %v", err)
	}

	// Deleting the key signs out every browser that used it, at once, and
	// the sweep then clears what's left.
	s.revokeKey(key)
	if _, err := store.ResumeSession(ctx, second); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("ResumeSession() after the key was deleted error = %v, want ErrInvalidToken", err)
	}
	if deleted, err := store.SweepSessions(ctx, now); err != nil || deleted != 1 {
		t.Fatalf("SweepSessions() after key deletion = %d, %v; want 1", deleted, err)
	}

	// One key holds at most MaxSessionsPerKey; signing in past it ends the
	// oldest.
	other, err := store.IssueKey(ctx, "u2", "free")
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for range MaxSessionsPerKey + 2 {
		advance(time.Second)
		token, _, err := store.CreateSession(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	for i, token := range tokens {
		_, err := store.ResumeSession(ctx, token)
		if old := i < 2; old != errors.Is(err, ErrInvalidToken) {
			t.Errorf("session %d of %d: ResumeSession error = %v", i+1, len(tokens), err)
		}
	}
}
