//go:build integration

package moderation

import (
	"context"
	"testing"
	"time"

	"neochat/internal/dbtest"
)

func TestPostgresBlockLog_RecordsAndReturnsEntries(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "moderation_block_log")
	log := NewPostgresBlockLog(pgDB)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	mustLog(t, log.Record(ctx, BlockEntry{UserID: "u1", Categories: []string{"illegal_activity"}, Reason: "asks how to commit a crime", Timestamp: now}))
	mustLog(t, log.Record(ctx, BlockEntry{UserID: "u2", Categories: []string{"self_harm"}, Reason: "seeks self-harm help", Timestamp: now}))

	entries, err := log.entriesForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].UserID != "u1" || entries[1].UserID != "u2" {
		t.Errorf("unexpected entry order/content: %+v", entries)
	}
	if entries[0].Categories[0] != "illegal_activity" {
		t.Errorf("Categories = %v, want [illegal_activity]", entries[0].Categories)
	}
	if !entries[0].Timestamp.Equal(now) {
		t.Errorf("Timestamp = %v, want %v", entries[0].Timestamp, now)
	}
}
