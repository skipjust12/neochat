//go:build integration

package costlog

import (
	"context"
	"testing"
	"time"

	"neochat/internal/dbtest"
	"neochat/router"
)

func TestPostgresStore_RecordThenEntries(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "cost_log")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	e1 := router.CostLogEntry{UserID: "u1", RequestID: "r1", ModelID: "gpt-5.6-terra", Mode: "thinking", InputTokens: 100, OutputTokens: 50, CostUSD: 0.0004, Timestamp: now}
	e2 := router.CostLogEntry{UserID: "u1", RequestID: "r2", ModelID: "deepseek-v4-flash", Mode: "instant", InputTokens: 10, OutputTokens: 5, CostUSD: 0.000002, Timestamp: now}

	if err := store.Record(ctx, e1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Record(ctx, e2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := store.entriesForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(got))
	}
	for i, want := range []router.CostLogEntry{e1, e2} {
		g := got[i]
		if g.UserID != want.UserID || g.RequestID != want.RequestID || g.ModelID != want.ModelID ||
			g.Mode != want.Mode || g.InputTokens != want.InputTokens || g.OutputTokens != want.OutputTokens ||
			g.CostUSD != want.CostUSD || !g.Timestamp.Equal(want.Timestamp) {
			t.Errorf("entries[%d] = %+v, want %+v", i, g, want)
		}
	}
}

func TestPostgresStore_EmptyStore(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "cost_log")
	store := NewPostgresStore(pgDB)

	got, err := store.entriesForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("entries on an empty store = %+v, want empty", got)
	}
}
