package limits

import (
	"context"
	"fmt"
	"time"
)

// rollingWindow is the single window this package checks against, per
// docs/unit-economics.md section 6.2's decision to drop the shorter
// 5h/7d sub-windows for v1 in favor of one 30-day rolling cap.
const rollingWindow = 30 * 24 * time.Hour

// CheckThinkingMaxLock reports whether userID's Thinking+Max spend over
// the trailing 30 days has reached their plan's hard cap. When true, the
// caller must route this request with router.Route's thinkingMaxLocked
// parameter set to true, forcing Thinking/Max down to Instant -- see
// docs/unit-economics.md section 6.4. The lock clears on its own as old
// spend ages out of the rolling window; there is no separate reset step.
func CheckThinkingMaxLock(ctx context.Context, store SpendStore, plan PlanLimits, userID string) (bool, error) {
	spent, err := store.Sum(ctx, userID, PoolThinkingMax, rollingWindow)
	if err != nil {
		return false, fmt.Errorf("limits: sum thinking_max spend: %w", err)
	}
	return spent >= plan.ThinkingMaxCapUSD, nil
}

// RecordThinkingMaxSpend appends an actual Thinking/Max generation cost to
// userID's rolling total. Call after generation completes, with the real
// cost (router.ComputeCostUSD against actual token usage) -- not
// RouteResult.EstimatedCostUSD, which is only a pre-flight guess.
func RecordThinkingMaxSpend(ctx context.Context, store SpendStore, userID string, costUSD float64, at time.Time) error {
	return store.Record(ctx, userID, PoolThinkingMax, costUSD, at)
}

// CheckInstantOverCap reports whether userID's Instant spend over the
// trailing 30 days has reached their plan's InstantExtraCapUSD. Unlike
// CheckThinkingMaxLock, this is never wired into router.Route -- Instant
// is already the cheapest tier, there is nothing to downgrade to. It
// exists purely for a gateway-side throttle (queue/delay requests, do not
// block them) as described in docs/unit-economics.md section 6.5; that
// throttling behavior itself is out of scope for this package.
func CheckInstantOverCap(ctx context.Context, store SpendStore, plan PlanLimits, userID string) (bool, error) {
	spent, err := store.Sum(ctx, userID, PoolInstant, rollingWindow)
	if err != nil {
		return false, fmt.Errorf("limits: sum instant spend: %w", err)
	}
	return spent >= plan.InstantExtraCapUSD, nil
}

// RecordInstantSpend appends an actual Instant generation cost to userID's
// rolling total. Call after generation completes, with the real cost.
func RecordInstantSpend(ctx context.Context, store SpendStore, userID string, costUSD float64, at time.Time) error {
	return store.Record(ctx, userID, PoolInstant, costUSD, at)
}
