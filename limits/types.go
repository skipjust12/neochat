// Package limits tracks per-user spend against the plan caps described in
// docs/unit-economics.md section 6, and answers the one question
// router.Route needs to enforce them: "is Thinking+Max locked for this
// user right now?" It knows nothing about model selection (that's
// router/) and nothing about how spend is actually persisted (that's
// SpendStore) -- this package is the accounting layer in between.
package limits

import (
	"context"
	"time"
)

// Pool identifies which spending pool a cost belongs to. Instant and
// Thinking+Max are tracked separately and never share a budget -- see
// docs/unit-economics.md section 6.1 for why (Instant is near-free and
// meant to feel unlimited; Thinking+Max is the actual constrained
// resource).
type Pool string

const (
	PoolInstant     Pool = "instant"
	PoolThinkingMax Pool = "thinking_max"
)

// SpendStore tracks per-user spend over rolling time windows. It is the
// only seam between this package and wherever spend actually lives.
// InMemorySpendStore (store.go) is a deliberately throwaway stand-in --
// good enough to develop and test against, with no real persistence.
//
// Swap-in procedure once a real server/database exists: implement this
// interface against Postgres/Redis (the bucketing scheme in store.go's
// doc comment carries over directly to either), point callers at the new
// implementation instead of NewInMemorySpendStore, and delete store.go.
// Nothing in limits.go or router/ depends on which implementation is in
// use, only on this interface.
type SpendStore interface {
	// Sum returns the total cost recorded for userID in pool within the
	// last `window` -- a rolling window ending now, not a fixed calendar
	// period (see docs/unit-economics.md section 6.2 on why rolling).
	Sum(ctx context.Context, userID string, pool Pool, window time.Duration) (float64, error)

	// Record appends an actual spend entry, timestamped at `at`. Call this
	// after generation completes with the real cost -- build it with
	// router.ComputeCostUSD from actual token usage, not
	// RouteResult.EstimatedCostUSD, which is only a pre-flight guess.
	Record(ctx context.Context, userID string, pool Pool, costUSD float64, at time.Time) error
}

// PlanLimits is one subscription plan's monthly ceilings, loaded from
// configs/plans.json. router/ never sees this type -- it has no notion of
// "plans", only the boolean CheckThinkingMaxLock derives from it.
type PlanLimits struct {
	PlanID string `json:"plan_id"`

	// ThinkingMaxCapUSD is the 30-day rolling hard cap on Thinking+Max
	// spend (docs/unit-economics.md section 6.3). Reaching it forces
	// Thinking/Max requests down to Instant until the window rolls off --
	// see CheckThinkingMaxLock.
	ThinkingMaxCapUSD float64 `json:"thinking_max_cap_usd"`

	// InstantExtraCapUSD is the 30-day rolling ceiling on Instant spend.
	// Purely a gateway-side anti-bot throttle -- it never reaches
	// router.Route (Instant is already the cheapest tier, there is
	// nothing to downgrade to). See docs/unit-economics.md section 6.5.
	InstantExtraCapUSD float64 `json:"instant_extra_cap_usd"`
}
