// Package idempotency guards against duplicate billing when a client
// retries a request after a dropped connection -- see README pre-launch
// checklist item 3 ("Idempotency itself is still unaddressed"). A client
// that never saw a response for a /chat call has no way to know whether
// generation (and billing) actually happened, so it must be safe for it to
// retry with the same idempotency key and get the original result back
// instead of triggering a second generate call and a second charge.
package idempotency

import (
	"context"
	"errors"
)

// ErrInFlight is returned by Store.Reserve when another attempt for the
// same (user_id, key) is currently being processed -- e.g. a client fired a
// retry before the original request could possibly have finished, or two
// concurrent requests raced with the same key. The caller should reject
// this rather than wait: waiting here would just move the duplicate-
// request problem into the HTTP layer's timeout instead of resolving it.
var ErrInFlight = errors.New("idempotency: request already in flight")

// Record is the cached result of a completed attempt, keyed by
// (user_id, idempotency_key). Response is opaque to this package -- the
// server layer marshals/unmarshals its own response type into it.
type Record struct {
	Response []byte
}

// Store is the interface seam between this package's dedup bookkeeping and
// wherever it actually lives. InMemoryStore is a deliberately throwaway
// stand-in, the same role limits.SpendStore/conversation.Store/
// moderation.BlockLog play for their own data.
//
// Swap-in procedure once a real database exists: implement this interface
// against Redis (SET key NX for Reserve's claim, matching the "in flight"
// marker; SET with the response body plus a TTL for Complete; DEL for
// Release) so state is shared across instances instead of one process --
// the same reasoning as those other stores' doc comments. A TTL on
// completed entries (this in-memory version keeps them forever) is worth
// adding at that point too, since a real deployment's request volume would
// otherwise grow this store without bound.
type Store interface {
	// Reserve claims (userID, key) for a new attempt.
	//
	//   - Never seen before: marks it in-flight and returns
	//     (Record{}, false, nil) -- the caller should proceed with the
	//     actual work and report back via Complete or Release.
	//   - Already completed: returns (the stored Record, true, nil) -- the
	//     caller should return that instead of doing the work again.
	//   - Currently in-flight (a concurrent or retried request got there
	//     first and hasn't finished): returns (Record{}, false, ErrInFlight).
	Reserve(ctx context.Context, userID, key string) (Record, bool, error)

	// Complete stores the finished result and clears the in-flight marker,
	// so a later Reserve for the same key returns it instead of running
	// the work again.
	Complete(ctx context.Context, userID, key string, record Record) error

	// Release clears the in-flight marker without storing a result --
	// used when the attempt failed, so a legitimate retry after a real
	// failure isn't permanently stuck behind ErrInFlight.
	Release(ctx context.Context, userID, key string) error
}
