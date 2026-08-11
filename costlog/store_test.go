package costlog

import (
	"context"
	"testing"
	"time"

	"neochat/router"
)

func TestInMemoryStore_RecordThenEntries(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	e1 := router.CostLogEntry{UserID: "u1", RequestID: "r1", ModelID: "gpt-5.6-terra", Mode: "thinking", InputTokens: 100, OutputTokens: 50, CostUSD: 0.0004, Timestamp: time.Now()}
	e2 := router.CostLogEntry{UserID: "u1", RequestID: "r2", ModelID: "deepseek-v4-flash", Mode: "instant", InputTokens: 10, OutputTokens: 5, CostUSD: 0.000002, Timestamp: time.Now()}

	if err := store.Record(ctx, e1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Record(ctx, e2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := store.Entries()
	if len(got) != 2 {
		t.Fatalf("len(Entries()) = %d, want 2", len(got))
	}
	if got[0] != e1 || got[1] != e2 {
		t.Errorf("Entries() = %+v, want [%+v, %+v] in insertion order", got, e1, e2)
	}
}

func TestInMemoryStore_EntriesReturnsACopy(t *testing.T) {
	store := NewInMemoryStore()
	if err := store.Record(context.Background(), router.CostLogEntry{UserID: "u1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := store.Entries()
	got[0].UserID = "mutated"

	if store.Entries()[0].UserID != "u1" {
		t.Error("mutating the slice returned by Entries() must not affect the store's internal state")
	}
}

func TestInMemoryStore_EmptyStore(t *testing.T) {
	store := NewInMemoryStore()
	if got := store.Entries(); len(got) != 0 {
		t.Errorf("Entries() on an empty store = %+v, want empty", got)
	}
}
