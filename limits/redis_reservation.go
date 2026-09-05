package limits

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Existing hourly spend keys are retained: deployment must not reset budgets.
var reserveScript = redis.NewScript(`
 if redis.call('GET', KEYS[1]) then return 1 end
 local total = 0
 for i=2,#KEYS do total = total + tonumber(redis.call('GET', KEYS[i]) or '0') end
 if total + tonumber(ARGV[1]) > tonumber(ARGV[2]) then return 0 end
 redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[3])
 redis.call('INCRBYFLOAT', KEYS[#KEYS], ARGV[1])
 redis.call('EXPIRE', KEYS[#KEYS], ARGV[3])
 return 1
`)

var settleScript = redis.NewScript(`
 local reserved = redis.call('GET', KEYS[1])
 if not reserved then return 0 end
 if not redis.call('GET', KEYS[2]) then return redis.error_reply('missing spend bucket') end
 redis.call('INCRBYFLOAT', KEYS[2], tonumber(ARGV[1]) - tonumber(reserved))
 redis.call('DEL', KEYS[1])
 return 1
`)

func reservationKey(r Reservation) string {
	return fmt.Sprintf("reservation:%s:%s:%s", r.UserID, r.Pool, r.ID)
}

func (s *RedisSpendStore) Reserve(ctx context.Context, userID string, pool Pool, amount, cap float64) (Reservation, error) {
	r, err := newReservation(userID, pool, amount, cap)
	if err != nil {
		return r, err
	}
	keys := []string{reservationKey(r)}
	for b := bucketIndex(r.At.Add(-rollingWindow)); b <= bucketIndex(r.At); b++ {
		keys = append(keys, spendKey(userID, pool, b))
	}
	ok, err := reserveScript.Run(ctx, s.rdb, keys, amount, cap, int64(maxRetention/time.Second)).Int()
	if err != nil {
		return Reservation{}, fmt.Errorf("limits: reserve budget: %w", err)
	}
	if ok == 0 {
		return Reservation{}, ErrBudgetExceeded
	}
	return r, nil
}

func (s *RedisSpendStore) Settle(ctx context.Context, r Reservation, actual float64) error {
	if !validMoney(actual) {
		return fmt.Errorf("limits: invalid actual cost")
	}
	return settleScript.Run(ctx, s.rdb, []string{reservationKey(r), spendKey(r.UserID, r.Pool, bucketIndex(r.At))}, actual).Err()
}
