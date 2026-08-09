package limits

import (
	"context"
	"testing"
	"time"
)

func TestInMemorySpendStore_SumWithinWindow(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySpendStore()
	now := time.Now()

	if err := store.Record(ctx, "u1", PoolThinkingMax, 2.5, now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Record(ctx, "u1", PoolThinkingMax, 1.5, now.Add(-time.Hour)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sum, err := store.Sum(ctx, "u1", PoolThinkingMax, 24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum != 4.0 {
		t.Errorf("Sum() = %.4f, want 4.0", sum)
	}
}

func TestInMemorySpendStore_ExcludesEntriesOutsideWindow(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySpendStore()
	now := time.Now()

	if err := store.Record(ctx, "u1", PoolThinkingMax, 10.0, now.Add(-40*24*time.Hour)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Record(ctx, "u1", PoolThinkingMax, 1.0, now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sum, err := store.Sum(ctx, "u1", PoolThinkingMax, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum != 1.0 {
		t.Errorf("Sum() = %.4f, want 1.0 (the 40-day-old entry must be excluded from a 30-day window)", sum)
	}
}

func TestInMemorySpendStore_PoolsAndUsersAreIsolated(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySpendStore()
	now := time.Now()

	must(t, store.Record(ctx, "u1", PoolThinkingMax, 5.0, now))
	must(t, store.Record(ctx, "u1", PoolInstant, 1.0, now))
	must(t, store.Record(ctx, "u2", PoolThinkingMax, 9.0, now))

	sum, err := store.Sum(ctx, "u1", PoolThinkingMax, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum != 5.0 {
		t.Errorf("u1/thinking_max Sum() = %.4f, want 5.0 (must not include u1/instant or u2/thinking_max)", sum)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
