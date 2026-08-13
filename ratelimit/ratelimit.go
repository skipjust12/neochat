// Package ratelimit throttles how often a given identity (an IP address,
// today -- see server.Server.IPRateLimiter) may hit the API, independent
// of and complementary to limits.SpendStore's dollar-based caps. Where
// limits/ answers "has this user_id spent too much", this package answers
// a cheaper, coarser question -- "has this identity made too many
// requests, recently" -- that matters even when a request is free or the
// caller lies about who it is (see audit.md's finding #1 on user_id not
// being authenticated: an IP-keyed limit is the one control here that
// doesn't rely on trusting a client-supplied identity at all).
package ratelimit

import "context"

// Limiter reports whether one more request identified by key is allowed
// right now. Calling Allow counts as consuming one request against key's
// quota -- it is not a peek. A "no" (allowed == false, err == nil) means
// the caller should reject the request outright, not retry the check.
type Limiter interface {
	Allow(ctx context.Context, key string) (bool, error)
}
