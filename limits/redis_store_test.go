//go:build integration

package limits

import (
	"context"
	"testing"
	"time"

	"neochat/internal/dbtest"
)

func TestRedisSpendStore_SumWithinWindow(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "spend:ru1:")
	store := NewRedisSpendStore(rdb)
	ctx := context.Background()
	now := time.Now()

	if err := store.Record(ctx, "ru1", PoolThinkingMax, 2.5, now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Record(ctx, "ru1", PoolThinkingMax, 1.5, now.Add(-time.Hour)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sum, err := store.Sum(ctx, "ru1", PoolThinkingMax, 24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum != 4.0 {
		t.Errorf("Sum() = %.4f, want 4.0", sum)
	}
}

func TestRedisSpendStore_ExcludesEntriesOutsideWindow(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "spend:ru2:")
	store := NewRedisSpendStore(rdb)
	ctx := context.Background()
	now := time.Now()

	if err := store.Record(ctx, "ru2", PoolThinkingMax, 10.0, now.Add(-40*24*time.Hour)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Record(ctx, "ru2", PoolThinkingMax, 1.0, now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sum, err := store.Sum(ctx, "ru2", PoolThinkingMax, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum != 1.0 {
		t.Errorf("Sum() = %.4f, want 1.0 (the 40-day-old entry must be excluded from a 30-day window)", sum)
	}
}

func TestRedisSpendStore_PoolsAndUsersAreIsolated(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "spend:ru3:", "spend:ru4:")
	store := NewRedisSpendStore(rdb)
	ctx := context.Background()
	now := time.Now()

	must(t, store.Record(ctx, "ru3", PoolThinkingMax, 5.0, now))
	must(t, store.Record(ctx, "ru3", PoolInstant, 1.0, now))
	must(t, store.Record(ctx, "ru4", PoolThinkingMax, 9.0, now))

	sum, err := store.Sum(ctx, "ru3", PoolThinkingMax, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum != 5.0 {
		t.Errorf("ru3/thinking_max Sum() = %.4f, want 5.0 (must not include ru3/instant or ru4/thinking_max)", sum)
	}
}
