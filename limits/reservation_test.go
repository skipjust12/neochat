package limits

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func exerciseReservations(t *testing.T, store SpendStore) {
	t.Helper()
	ctx := context.Background()
	user := fmtUser(t)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Reserve(ctx, user, PoolInstant, 1, 3); err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrBudgetExceeded) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("accepted %d requests for budget 3", accepted.Load())
	}
	r, err := store.Reserve(ctx, user, PoolThinkingMax, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Settle(ctx, r, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := store.Settle(ctx, r, 0); err != nil {
		t.Fatal(err)
	}
	spent, err := store.Sum(ctx, user, PoolThinkingMax, 30*24*time.Hour)
	if err != nil || math.Abs(spent-0.5) > 1e-12 {
		t.Fatalf("double refund or lost spend: %f %v", spent, err)
	}
	if _, err := store.Reserve(ctx, user, PoolThinkingMax, 3, 3); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("ignored prior spend: %v", err)
	}
}

func fmtUser(t *testing.T) string {
	r, err := newReservation("test", PoolInstant, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}
func TestMemoryReservationsAreAtomic(t *testing.T) { exerciseReservations(t, NewInMemorySpendStore()) }
