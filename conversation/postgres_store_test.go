//go:build integration

package conversation

import (
	"context"
	"testing"
	"time"

	"neochat/internal/dbtest"
)

func TestPostgresStore_AppendAndHistoryPreserveOrder(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "hi", CreatedAt: time.Now().UTC()}))
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleAssistant, Content: "hello", ModelID: "m1", CreatedAt: time.Now().UTC()}))

	history, err := store.History(ctx, "u1", "c1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("len(History()) = %d, want 2", len(history))
	}
	if history[0].Role != RoleUser || history[0].Content != "hi" {
		t.Errorf("unexpected first message: %+v", history[0])
	}
	if history[1].Role != RoleAssistant || history[1].ModelID != "m1" {
		t.Errorf("unexpected second message: %+v", history[1])
	}
}

func TestPostgresStore_UnknownConversationReturnsEmptyNotError(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	history, err := store.History(ctx, "u1", "does-not-exist", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("len(History()) = %d, want 0", len(history))
	}
}

func TestPostgresStore_IsolatesByUserAndConversation(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "u1/c1", CreatedAt: time.Now().UTC()}))
	must(t, store.Append(ctx, "u2", "c1", Message{Role: RoleUser, Content: "u2/c1", CreatedAt: time.Now().UTC()}))
	must(t, store.Append(ctx, "u1", "c2", Message{Role: RoleUser, Content: "u1/c2", CreatedAt: time.Now().UTC()}))

	h, err := store.History(ctx, "u1", "c1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(h) != 1 || h[0].Content != "u1/c1" {
		t.Errorf("u1/c1 history = %+v, want exactly [u1/c1]", h)
	}
}

func TestPostgresStore_GetSummaryUnknownConversationReturnsZeroValue(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	s, err := store.GetSummary(ctx, "u1", "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Text != "" || s.CoversThrough != 0 {
		t.Errorf("GetSummary() = %+v, want the zero Summary", s)
	}
}

func TestPostgresStore_SetSummaryThenGetSummaryRoundTrips(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	want := Summary{Text: "the user asked about X and Y", CoversThrough: 5, UpdatedAt: time.Now().UTC()}
	must(t, store.SetSummary(ctx, "u1", "c1", want))

	got, err := store.GetSummary(ctx, "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != want.Text || got.CoversThrough != want.CoversThrough {
		t.Errorf("GetSummary() = %+v, want %+v", got, want)
	}

	// Round 2: SetSummary must upsert, not fail on a second call for the
	// same (user_id, conversation_id).
	want2 := Summary{Text: "updated summary", CoversThrough: 9, UpdatedAt: time.Now().UTC()}
	must(t, store.SetSummary(ctx, "u1", "c1", want2))
	got2, err := store.GetSummary(ctx, "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got2.Text != want2.Text || got2.CoversThrough != want2.CoversThrough {
		t.Errorf("GetSummary() after update = %+v, want %+v", got2, want2)
	}
}

func TestPostgresStore_SummaryIsolatesByUserAndConversation(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.SetSummary(ctx, "u1", "c1", Summary{Text: "u1/c1 summary", UpdatedAt: time.Now().UTC()}))
	must(t, store.SetSummary(ctx, "u2", "c1", Summary{Text: "u2/c1 summary", UpdatedAt: time.Now().UTC()}))

	got, err := store.GetSummary(ctx, "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "u1/c1 summary" {
		t.Errorf("GetSummary(u1, c1) = %+v, want Text=%q", got, "u1/c1 summary")
	}
}
