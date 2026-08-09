package limits

import (
	"context"
	"testing"
	"time"
)

func testPlan() PlanLimits {
	return PlanLimits{PlanID: "pro", ThinkingMaxCapUSD: 10.0, InstantExtraCapUSD: 3.0}
}

func TestCheckThinkingMaxLock(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySpendStore()
	plan := testPlan()

	locked, err := CheckThinkingMaxLock(ctx, store, plan, "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if locked {
		t.Error("expected unlocked with no spend recorded")
	}

	if err := RecordThinkingMaxSpend(ctx, store, "u1", 9.99, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	locked, err = CheckThinkingMaxLock(ctx, store, plan, "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if locked {
		t.Error("expected unlocked just under the $10 cap")
	}

	if err := RecordThinkingMaxSpend(ctx, store, "u1", 0.02, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	locked, err = CheckThinkingMaxLock(ctx, store, plan, "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !locked {
		t.Error("expected locked once spend crosses the $10 cap")
	}
}

func TestCheckThinkingMaxLock_UnlocksAsSpendAgesOut(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySpendStore()
	plan := testPlan()
	now := time.Now()

	if err := RecordThinkingMaxSpend(ctx, store, "u1", 10.0, now.Add(-31*24*time.Hour)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	locked, err := CheckThinkingMaxLock(ctx, store, plan, "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if locked {
		t.Error("expected the lock to clear on its own once the spend that caused it rolls off the 30-day window")
	}
}

func TestCheckInstantOverCap(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySpendStore()
	plan := testPlan()

	if err := RecordInstantSpend(ctx, store, "u1", 3.5, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	over, err := CheckInstantOverCap(ctx, store, plan, "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !over {
		t.Error("expected over cap at $3.5 spent against a $3 instant_extra_cap_usd")
	}

	// Thinking+Max spend must never count against the Instant cap -- the
	// two pools are independent (docs/unit-economics.md section 6.1).
	thinkingMaxLocked, err := CheckThinkingMaxLock(ctx, store, plan, "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if thinkingMaxLocked {
		t.Error("instant spend must not count against the thinking_max cap")
	}
}
