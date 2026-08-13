package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestInMemoryLimiter_AllowsUpToLimit(t *testing.T) {
	ctx := context.Background()
	l := NewInMemoryLimiter(3, time.Minute)

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

func TestInMemoryLimiter_KeysAreIsolated(t *testing.T) {
	ctx := context.Background()
	l := NewInMemoryLimiter(1, time.Minute)

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

func TestInMemoryLimiter_ResetsOnNewWindow(t *testing.T) {
	l := NewInMemoryLimiter(1, time.Second)
	// bucket is derived from time.Now() inside Allow, so this test can't
	// inject a fake clock without one -- instead it picks a window short
	// enough (1s) that sleeping past it is fast and reliable.
	ctx := context.Background()

	if allowed, err := l.Allow(ctx, "ip1"); err != nil || !allowed {
		t.Fatalf("1st request: allowed=%v err=%v, want true/nil", allowed, err)
	}
	if allowed, err := l.Allow(ctx, "ip1"); err != nil || allowed {
		t.Fatalf("2nd request (same window): allowed=%v err=%v, want false/nil", allowed, err)
	}

	time.Sleep(1100 * time.Millisecond)

	if allowed, err := l.Allow(ctx, "ip1"); err != nil || !allowed {
		t.Fatalf("1st request in new window: allowed=%v err=%v, want true/nil", allowed, err)
	}
}
