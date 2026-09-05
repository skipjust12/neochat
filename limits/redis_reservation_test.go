//go:build integration

package limits

import (
	"context"
	"errors"
	"neochat/internal/dbtest"
	"testing"
	"time"
)

func TestRedisReservationsAreAtomic(t *testing.T) {
	exerciseReservations(t, NewRedisSpendStore(dbtest.Redis(t)))
}
func TestRedisReservationsIncludeLegacySpend(t *testing.T) {
	store := NewRedisSpendStore(dbtest.Redis(t))
	ctx := context.Background()
	user := fmtUser(t)
	if err := store.Record(ctx, user, PoolInstant, 2, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(ctx, user, PoolInstant, 2, 3); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("legacy budget reset: %v", err)
	}
}
