package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLimiter is a Limiter backed by Redis: one counter key per (prefix,
// key, fixed-window bucket), INCR on every Allow call, with the key's TTL
// set the first time a bucket is touched so an idle key expires on its
// own instead of needing InMemoryLimiter's prune -- same scheme
// limits.RedisSpendStore uses for spend, counting requests instead of
// summing cost.
type RedisLimiter struct {
	rdb    *redis.Client
	prefix string
	Limit  int
	Window time.Duration
}

// NewRedisLimiter returns a Limiter backed by rdb, allowing up to limit
// requests per key within each Window-wide fixed window. prefix
// namespaces this limiter's keys from any other RedisLimiter sharing the
// same Redis instance (e.g. an IP limiter vs. a future per-user_id one).
func NewRedisLimiter(rdb *redis.Client, prefix string, limit int, window time.Duration) *RedisLimiter {
	return &RedisLimiter{rdb: rdb, prefix: prefix, Limit: limit, Window: window}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string) (bool, error) {
	bucket := time.Now().Unix() / int64(l.Window/time.Second)
	redisKey := fmt.Sprintf("ratelimit:%s:%s:%d", l.prefix, key, bucket)

	count, err := l.rdb.Incr(ctx, redisKey).Result()
	if err != nil {
		return false, fmt.Errorf("ratelimit: redis incr %s: %w", redisKey, err)
	}
	if count == 1 {
		// Only set on the bucket's first increment (count==1 means this
		// call created the key) -- an unconditional re-set on every call,
		// the way RedisSpendStore.Record does for its own reasons, would
		// keep pushing a hot key's expiry forward and defeat the fixed
		// window.
		if err := l.rdb.Expire(ctx, redisKey, l.Window).Err(); err != nil {
			return false, fmt.Errorf("ratelimit: redis expire %s: %w", redisKey, err)
		}
	}
	return count <= int64(l.Limit), nil
}
