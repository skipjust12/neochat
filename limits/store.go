package limits

import (
	"context"
	"sync"
	"time"
)

// bucketWidth is the resolution spend is aggregated at. An hour is precise
// enough for the 30-day window this package actually checks (error is
// bounded to one bucket width, well under 1% of a 30-day window) -- see
// docs/unit-economics.md section 6.2, which dropped the shorter 5h/7d
// windows that would have needed finer buckets.
const bucketWidth = time.Hour

// maxRetention is how long a bucket is kept before prune() drops it: the
// longest window this package checks (30 days) plus a day of slack.
const maxRetention = 31 * 24 * time.Hour

// InMemorySpendStore is a process-local SpendStore backed by a plain Go
// map, safe for concurrent use. It is intentionally throwaway: nothing
// persists across a restart, and there is no cross-instance sharing --
// exactly as much "database" as makes sense before there is a server to
// give one a job to do. See the SpendStore doc comment in types.go for
// the swap-in procedure once real persistence is needed.
//
// Spend is tracked in fixed-width time buckets rather than one entry per
// request, so Sum stays O(window/bucketWidth) instead of growing with
// request volume -- the same scheme a Redis-backed store would use
// (INCRBYFLOAT per bucket key with a TTL), just against a map instead of
// real keys.
type InMemorySpendStore struct {
	mu      sync.Mutex
	buckets map[bucketKey]float64
}

type bucketKey struct {
	userID string
	pool   Pool
	bucket int64
}

// NewInMemorySpendStore returns an empty, ready-to-use store.
func NewInMemorySpendStore() *InMemorySpendStore {
	return &InMemorySpendStore{buckets: make(map[bucketKey]float64)}
}

func bucketIndex(t time.Time) int64 {
	return t.Unix() / int64(bucketWidth/time.Second)
}

func (s *InMemorySpendStore) Record(_ context.Context, userID string, pool Pool, costUSD float64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := bucketKey{userID: userID, pool: pool, bucket: bucketIndex(at)}
	s.buckets[key] += costUSD
	s.prune(at)
	return nil
}

func (s *InMemorySpendStore) Sum(_ context.Context, userID string, pool Pool, window time.Duration) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	oldest := bucketIndex(now.Add(-window))
	newest := bucketIndex(now)

	var total float64
	for b := oldest; b <= newest; b++ {
		total += s.buckets[bucketKey{userID: userID, pool: pool, bucket: b}]
	}
	return total, nil
}

// prune drops buckets older than maxRetention so the map doesn't grow
// forever in a long-running process. Runs opportunistically on every
// write (a full scan -- fine at this store's intended scale of "dev
// stand-in", not something a real backing store would do; a real store
// uses per-key TTLs instead, see the SpendStore doc comment).
func (s *InMemorySpendStore) prune(at time.Time) {
	cutoff := bucketIndex(at.Add(-maxRetention))
	for k := range s.buckets {
		if k.bucket < cutoff {
			delete(s.buckets, k)
		}
	}
}
