package idempotency

import (
	"context"
	"errors"
	"testing"
)

func exerciseStore(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	user, err := newOwner()
	if err != nil {
		t.Fatal(err)
	}
	lease, found, err := store.Reserve(ctx, user, "key")
	if err != nil || found || lease.Owner == "" {
		t.Fatalf("reserve: %+v %v %v", lease, found, err)
	}
	if _, _, err := store.Reserve(ctx, user, "key"); !errors.Is(err, ErrInFlight) {
		t.Fatalf("duplicate accepted: %v", err)
	}
	if err := store.Complete(ctx, user, "key", Record{Owner: "wrong", Response: []byte("wrong")}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong owner completed: %v", err)
	}
	if err := store.Release(ctx, user, "key", "wrong"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong owner released: %v", err)
	}
	if err := store.Release(ctx, user, "key", lease.Owner); err != nil {
		t.Fatal(err)
	}
	replacement, _, err := store.Reserve(ctx, user, "key")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Release(ctx, user, "key", lease.Owner); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale owner released replacement: %v", err)
	}
	if err := store.Complete(ctx, user, "key", Record{Owner: lease.Owner, Response: []byte("stale")}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale owner completed replacement: %v", err)
	}
	if err := store.Complete(ctx, user, "key", Record{Owner: replacement.Owner, Response: []byte("answer")}); err != nil {
		t.Fatal(err)
	}
	cached, found, err := store.Reserve(ctx, user, "key")
	if err != nil || !found || string(cached.Response) != "answer" {
		t.Fatalf("replay: %+v %v %v", cached, found, err)
	}
	if _, found, err := store.Reserve(ctx, user+"other", "key"); err != nil || found {
		t.Fatalf("cross-user replay: %v %v", found, err)
	}
}

func TestInMemoryStore_OwnershipAndReplay(t *testing.T) { exerciseStore(t, NewInMemoryStore()) }
