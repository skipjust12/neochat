//go:build integration

package idempotency

import (
	"context"
	"errors"
	"testing"
	"time"

	"neochat/internal/dbtest"
)

func TestRedisStore_ReserveThenCompleteReplaysRecord(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "idem:u1:")
	store := NewRedisStore(rdb, time.Hour)
	ctx := context.Background()

	rec, found, err := store.Reserve(ctx, "u1", "key1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("first Reserve for a new key should report found=false, got Record %+v", rec)
	}

	want := Record{Response: []byte(`{"a":1}`)}
	if err := store.Complete(ctx, "u1", "key1", want); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, found, err := store.Reserve(ctx, "u1", "key1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("Reserve after Complete should report found=true")
	}
	if string(got.Response) != string(want.Response) {
		t.Errorf("Reserve() Record = %s, want %s", got.Response, want.Response)
	}
}

func TestRedisStore_ReserveWhileInFlightErrors(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "idem:u1:")
	store := NewRedisStore(rdb, time.Hour)
	ctx := context.Background()

	if _, found, err := store.Reserve(ctx, "u1", "key1"); err != nil || found {
		t.Fatalf("first Reserve: found=%v err=%v, want found=false err=nil", found, err)
	}

	if _, _, err := store.Reserve(ctx, "u1", "key1"); !errors.Is(err, ErrInFlight) {
		t.Errorf("second concurrent Reserve error = %v, want ErrInFlight", err)
	}
}

func TestRedisStore_ReleaseAllowsRetryAfterFailure(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "idem:u1:")
	store := NewRedisStore(rdb, time.Hour)
	ctx := context.Background()

	if _, _, err := store.Reserve(ctx, "u1", "key1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Release(ctx, "u1", "key1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, found, err := store.Reserve(ctx, "u1", "key1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("Reserve after Release should report found=false, not replay a stale record")
	}
}

func TestRedisStore_UsersAreIsolated(t *testing.T) {
	rdb := dbtest.Redis(t)
	dbtest.FlushRedis(t, rdb, "idem:u1:", "idem:u2:")
	store := NewRedisStore(rdb, time.Hour)
	ctx := context.Background()

	if err := store.Complete(ctx, "u1", "same-key", Record{Response: []byte("u1's response")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, found, err := store.Reserve(ctx, "u2", "same-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("u2 reserving the same key text as u1 should not see u1's completed record")
	}
}
