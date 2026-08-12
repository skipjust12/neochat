package limits

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisSpendStore is a SpendStore backed by Redis, using the exact
// bucketing scheme SpendStore's doc comment describes: one key per
// (userID, pool, hourly bucket), INCRBYFLOAT on Record, a TTL so old
// buckets expire on their own instead of needing InMemorySpendStore's
// prune().
type RedisSpendStore struct {
	rdb *redis.Client
}

// NewRedisSpendStore returns a SpendStore backed by rdb.
func NewRedisSpendStore(rdb *redis.Client) *RedisSpendStore {
	return &RedisSpendStore{rdb: rdb}
}

func spendKey(userID string, pool Pool, bucket int64) string {
	return fmt.Sprintf("spend:%s:%s:%d", userID, pool, bucket)
}

func (s *RedisSpendStore) Record(ctx context.Context, userID string, pool Pool, costUSD float64, at time.Time) error {
	key := spendKey(userID, pool, bucketIndex(at))
	if err := s.rdb.IncrByFloat(ctx, key, costUSD).Err(); err != nil {
		return fmt.Errorf("limits: redis incrbyfloat %s: %w", key, err)
	}
	// Re-set on every write rather than only on first write -- idempotent,
	// and cheap enough at this scale that it's not worth a conditional
	// EXPIRE (NX) round trip to check first.
	if err := s.rdb.Expire(ctx, key, maxRetention).Err(); err != nil {
		return fmt.Errorf("limits: redis expire %s: %w", key, err)
	}
	return nil
}

func (s *RedisSpendStore) Sum(ctx context.Context, userID string, pool Pool, window time.Duration) (float64, error) {
	now := time.Now()
	oldest := bucketIndex(now.Add(-window))
	newest := bucketIndex(now)

	keys := make([]string, 0, newest-oldest+1)
	for b := oldest; b <= newest; b++ {
		keys = append(keys, spendKey(userID, pool, b))
	}

	values, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return 0, fmt.Errorf("limits: redis mget: %w", err)
	}

	var total float64
	for _, v := range values {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("limits: parse spend value %q: %w", s, err)
		}
		total += f
	}
	return total, nil
}
