//go:build integration

package ratelimit

import (
	"context"
	"testing"
	"time"

	"neochat/internal/dbtest"
)

func TestRedisLimiter_AllowsUpToLimit(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "ratelimit:rt1:")
	l := NewRedisLimiter(rdb, "rt1", 3, time.Minute)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		allowed, err := l.Allow(ctx, "ip1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !allowed {
			t.Errorf("request %d: expected allowed within limit of 3", i+1)
		}
	}

	allowed, err := l.Allow(ctx, "ip1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("4th request: expected rejected, limit is 3 per window")
	}
}

func TestRedisLimiter_KeysAreIsolated(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "ratelimit:rt2:")
	l := NewRedisLimiter(rdb, "rt2", 1, time.Minute)
	ctx := context.Background()

	if allowed, err := l.Allow(ctx, "ip1"); err != nil || !allowed {
		t.Fatalf("ip1 1st request: allowed=%v err=%v, want true/nil", allowed, err)
	}
	if allowed, err := l.Allow(ctx, "ip2"); err != nil || !allowed {
		t.Fatalf("ip2 1st request: allowed=%v err=%v, want true/nil (must not share ip1's quota)", allowed, err)
	}
	if allowed, err := l.Allow(ctx, "ip1"); err != nil || allowed {
		t.Fatalf("ip1 2nd request: allowed=%v err=%v, want false/nil", allowed, err)
	}
}
