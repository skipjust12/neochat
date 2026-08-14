// Package ratelimit throttles how often a given identity may hit the
// API, independent of and complementary to limits.SpendStore's
// dollar-based caps. Where limits/ answers "has this user_id spent too
// much", this package answers a cheaper, coarser question -- "has this
// identity made too many requests, recently" -- which matters even when
// the requests are free.
//
// What counts as an identity is the caller's choice of key, and the
// server runs two Limiters over two different ones (see
// server.Server.IPRateLimiter and server.Server.UserRateLimiter): the
// client IP, which is available before any credential is checked and so
// is the only thing that can throttle a caller with no valid key at all,
// and the authenticated user_id, which follows one credential across
// however many addresses it's used from. Neither subsumes the other.
//
// A user_id-keyed limiter only became meaningful once user_id stopped
// being client-supplied (audit.md finding #1, fixed 2026-08-14); before
// that it would simply have moved with whatever user_id an abusive
// client claimed next request.
package ratelimit

import "context"

// Limiter reports whether one more request identified by key is allowed
// right now. Calling Allow counts as consuming one request against key's
// quota -- it is not a peek. A "no" (allowed == false, err == nil) means
// the caller should reject the request outright, not retry the check.
type Limiter interface {
	Allow(ctx context.Context, key string) (bool, error)
}
