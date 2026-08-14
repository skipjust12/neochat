// Package auth authenticates POST /chat and POST /chat/stream callers.
//
// Today's implementation (PostgresStore) is deliberately the simplest
// thing that closes audit.md finding #1 (no authentication at all,
// user_id/plan_id trusted straight out of the JSON body): opaque,
// non-expiring, per-user API keys, minted by a human operator via
// cmd/issuekey (see that command's doc comment) rather than any kind of
// signup/login flow, since neither the product nor its onboarding UX
// exists yet.
//
// The seam meant to survive a real auth system arriving later is
// Authenticator: server.Server depends only on its one method, not on
// PostgresStore or on how a token comes into being. A future JWT/session/
// OAuth system is a new Authenticator implementation (or a composite that
// tries the API-key store first and falls back to a session verifier) --
// server.go's authenticated middleware, decodeChatRequest, and every
// downstream package that already takes a resolved user_id/plan_id
// (limits/conversation/costlog/idempotency/...) stay untouched, because
// none of them have ever cared how UserID/PlanID were established, only
// that they're trustworthy by the time they see them. Swapping
// Authenticator implementations in cmd/server/main.go is the entire
// migration.
package auth

import (
	"context"
	"errors"
)

// Identity is the authenticated caller a valid credential resolves to --
// the user_id/plan_id server.go's request pipeline treats as ground
// truth from here on, replacing the unverified client-supplied user_id/
// plan_id request-body fields audit.md finding #1 describes.
type Identity struct {
	UserID string
	PlanID string
}

// ErrInvalidToken means the credential is missing, malformed, unknown, or
// otherwise not usable. Deliberately one error for all of those cases,
// not distinguished further, so a response built from it can't be used to
// tell "this key never existed" apart from "wrong format" -- see
// Authenticate implementations.
var ErrInvalidToken = errors.New("auth: invalid API key")

// Authenticator verifies a bearer credential and reports the Identity it
// belongs to, or ErrInvalidToken if it doesn't belong to anyone. This is
// the entire dependency server.Server has on authentication -- see the
// package doc comment for why that's deliberate.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Identity, error)
}

// Store is Authenticator plus key issuance -- the full read/write surface
// PostgresStore and InMemoryStore implement. server.Server only ever
// takes an Authenticator (see its Auth field); Store exists so
// cmd/issuekey and tests can mint keys against the same implementation
// server.Server authenticates against, without widening what server.go
// itself depends on.
type Store interface {
	Authenticator

	// IssueKey mints a fresh API key for (userID, planID) and returns the
	// raw token. The raw value is never stored or recoverable afterward --
	// see hashToken -- so losing it before the caller records it means
	// starting over with a new key.
	IssueKey(ctx context.Context, userID, planID string) (token string, err error)
}
