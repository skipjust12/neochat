//go:build integration

package idempotency

import (
	"context"
	"errors"
	"neochat/internal/dbtest"
	"testing"
	"time"
)

func TestRedisStore_OwnershipAndReplay(t *testing.T) {
	exerciseStore(t, NewRedisStore(dbtest.Redis(t), time.Hour))
}

func TestRedisStore_ExpiredOwnerCannotMutateReplacement(t *testing.T) {
	rdb := dbtest.Redis(t)
	store := NewRedisStore(rdb, time.Hour)
	ctx := context.Background()
	user, err := newOwner()
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := store.Reserve(ctx, user, "key")
	if err != nil {
		t.Fatal(err)
	}
	ttl, err := rdb.PTTL(ctx, idemKey(user, "key")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 12*time.Minute {
		t.Fatalf("lease expires before request deadline: %s", ttl)
	}
	if err := rdb.PExpire(ctx, idemKey(user, "key"), 0).Err(); err != nil {
		t.Fatal(err)
	}
	current, _, err := store.Reserve(ctx, user, "key")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Release(ctx, user, "key", old.Owner); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale release: %v", err)
	}
	if err := store.Complete(ctx, user, "key", Record{Owner: old.Owner}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale complete: %v", err)
	}
	if err := store.Complete(ctx, user, "key", Record{Owner: current.Owner, Response: []byte("new")}); err != nil {
		t.Fatal(err)
	}
}

func TestRedisStore_KeyComponentsCannotCollide(t *testing.T) {
	if idemKey("a:b", "c") == idemKey("a", "b:c") {
		t.Fatal("ambiguous key encoding")
	}
}
