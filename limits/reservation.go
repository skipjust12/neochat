package limits

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrBudgetExceeded = errors.New("spending limit reached: insufficient budget for this request")

// Reservation is a prepaid upper bound. Failed or interrupted calls keep this
// charge until usage is reconciled; losing a worker cannot restore its budget.
type Reservation struct {
	ID     string
	UserID string
	Pool   Pool
	Amount float64
	At     time.Time
}

func validMoney(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func newReservation(userID string, pool Pool, amount, cap float64) (Reservation, error) {
	if !validMoney(amount) || !validMoney(cap) || userID == "" || (pool != PoolInstant && pool != PoolThinkingMax) {
		return Reservation{}, fmt.Errorf("limits: invalid reservation")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Reservation{}, err
	}
	return Reservation{ID: hex.EncodeToString(id[:]), UserID: userID, Pool: pool, Amount: amount, At: time.Now()}, nil
}

func (s *InMemorySpendStore) Reserve(ctx context.Context, userID string, pool Pool, amount, cap float64) (Reservation, error) {
	if err := ctx.Err(); err != nil {
		return Reservation{}, err
	}
	r, err := newReservation(userID, pool, amount, cap)
	if err != nil {
		return r, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var total float64
	for b := bucketIndex(r.At.Add(-rollingWindow)); b <= bucketIndex(r.At); b++ {
		total += s.buckets[bucketKey{userID: userID, pool: pool, bucket: b}]
	}
	if total+amount > cap {
		return Reservation{}, ErrBudgetExceeded
	}
	s.buckets[bucketKey{userID: userID, pool: pool, bucket: bucketIndex(r.At)}] += amount
	if s.reservations == nil {
		s.reservations = make(map[string]Reservation)
	}
	s.reservations[r.ID] = r
	s.prune(r.At)
	return r, nil
}

func (s *InMemorySpendStore) Settle(ctx context.Context, r Reservation, actual float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validMoney(actual) {
		return fmt.Errorf("limits: invalid actual cost")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.reservations[r.ID]
	if !ok {
		return nil
	} // Already settled; a repeated completion cannot refund twice.
	if stored != r {
		return fmt.Errorf("limits: reservation mismatch")
	}
	s.buckets[bucketKey{userID: r.UserID, pool: r.Pool, bucket: bucketIndex(r.At)}] += actual - r.Amount
	delete(s.reservations, r.ID)
	return nil
}
