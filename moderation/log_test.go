package moderation

import (
	"context"
	"testing"
	"time"
)

func TestInMemoryBlockLog_RecordsAndReturnsEntries(t *testing.T) {
	ctx := context.Background()
	log := NewInMemoryBlockLog()
	now := time.Now()

	mustLog(t, log.Record(ctx, BlockEntry{UserID: "u1", Categories: []string{"illegal_activity"}, Reason: "asks how to commit a crime", Timestamp: now}))
	mustLog(t, log.Record(ctx, BlockEntry{UserID: "u2", Categories: []string{"self_harm"}, Reason: "seeks self-harm help", Timestamp: now}))

	entries := log.Entries()
	if len(entries) != 2 {
		t.Fatalf("len(Entries()) = %d, want 2", len(entries))
	}
	if entries[0].UserID != "u1" || entries[1].UserID != "u2" {
		t.Errorf("unexpected entry order/content: %+v", entries)
	}
	if entries[0].Categories[0] != "illegal_activity" {
		t.Errorf("Categories = %v, want [illegal_activity]", entries[0].Categories)
	}
}

func TestInMemoryBlockLog_EntriesReturnsACopy(t *testing.T) {
	ctx := context.Background()
	log := NewInMemoryBlockLog()
	mustLog(t, log.Record(ctx, BlockEntry{UserID: "u1", Timestamp: time.Now()}))

	entries := log.Entries()
	entries[0].UserID = "mutated"

	if log.Entries()[0].UserID != "u1" {
		t.Error("mutating the returned slice should not affect the log's internal state")
	}
}

func mustLog(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
