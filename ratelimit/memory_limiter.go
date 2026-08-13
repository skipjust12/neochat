package ratelimit

import (
	"context"
	"sync"
	"time"
)

// InMemoryLimiter is a process-local Limiter backed by a plain Go map,
// safe for concurrent use -- the same "throwaway, single instance" role
// limits.InMemorySpendStore plays for spend, for tests and any non-Redis
// path. Counts requests in fixed-width windows keyed on key, same
// bucketing idea as limits.InMemorySpendStore.
type InMemoryLimiter struct {
	Limit  int
	Window time.Duration

	mu      sync.Mutex
	buckets map[bucketKey]int
}

type bucketKey struct {
	key    string
	bucket int64
}

// NewInMemoryLimiter returns a ready-to-use Limiter allowing up to limit
// requests per key within each window-wide fixed window.
func NewInMemoryLimiter(limit int, window time.Duration) *InMemoryLimiter {
	return &InMemoryLimiter{Limit: limit, Window: window, buckets: make(map[bucketKey]int)}
}

func (l *InMemoryLimiter) Allow(_ context.Context, key string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	bk := bucketKey{key: key, bucket: time.Now().Unix() / int64(l.Window/time.Second)}
	l.buckets[bk]++
	l.prune(bk.bucket)
	return l.buckets[bk] <= l.Limit, nil
}

// prune drops buckets from windows before current so the map doesn't grow
// forever in a long-running process -- same opportunistic-full-scan
// tradeoff limits.InMemorySpendStore.prune makes, fine at this store's
// intended scale (dev/test, not a real deployment's traffic volume).
func (l *InMemoryLimiter) prune(current int64) {
	for k := range l.buckets {
		if k.bucket < current {
			delete(l.buckets, k)
		}
	}
}
