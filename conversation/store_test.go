package conversation

import (
	"context"
	"testing"
	"time"
)

func TestInMemoryStore_AppendAndHistoryPreserveOrder(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "hi", CreatedAt: time.Now()}))
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleAssistant, Content: "hello", ModelID: "m1", CreatedAt: time.Now()}))

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

func TestInMemoryStore_HistoryOffsetSkipsOldestMessages(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	for _, content := range []string{"m0", "m1", "m2", "m3"} {
		must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: content, CreatedAt: time.Now()}))
	}

	history, err := store.History(ctx, "u1", "c1", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("len(History(offset=2)) = %d, want 2", len(history))
	}
	// Index i of an offset-N read is absolute index N+i -- the property
	// Summary.CoversThrough arithmetic depends on (see Store.History).
	if history[0].Content != "m2" || history[1].Content != "m3" {
		t.Errorf("History(offset=2) = %q/%q, want m2/m3", history[0].Content, history[1].Content)
	}
}

func TestInMemoryStore_HistoryOffsetPastEndReturnsEmptyNotError(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "only", CreatedAt: time.Now()}))

	history, err := store.History(ctx, "u1", "c1", 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("len(History(offset past end)) = %d, want 0", len(history))
	}
}

func TestInMemoryStore_UnknownConversationReturnsEmptyNotError(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	history, err := store.History(ctx, "u1", "does-not-exist", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("len(History()) = %d, want 0", len(history))
	}
}

func TestInMemoryStore_IsolatesByUserAndConversation(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "u1/c1"}))
	must(t, store.Append(ctx, "u2", "c1", Message{Role: RoleUser, Content: "u2/c1"}))
	must(t, store.Append(ctx, "u1", "c2", Message{Role: RoleUser, Content: "u1/c2"}))

	h, err := store.History(ctx, "u1", "c1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(h) != 1 || h[0].Content != "u1/c1" {
		t.Errorf("u1/c1 history = %+v, want exactly [u1/c1]", h)
	}
}

func TestInMemoryStore_HistoryReturnsACopy(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "original"}))

	history, _ := store.History(ctx, "u1", "c1", 0)
	history[0].Content = "mutated"

	h2, _ := store.History(ctx, "u1", "c1", 0)
	if h2[0].Content != "original" {
		t.Error("mutating the returned slice should not affect the store's internal state")
	}
}

func TestInMemoryStore_GetSummaryUnknownConversationReturnsZeroValue(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	s, err := store.GetSummary(ctx, "u1", "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Text != "" || s.CoversThrough != 0 {
		t.Errorf("GetSummary() = %+v, want the zero Summary", s)
	}
}

func TestInMemoryStore_SetSummaryThenGetSummaryRoundTrips(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	want := Summary{Text: "the user asked about X and Y", CoversThrough: 5, UpdatedAt: time.Now()}
	must(t, store.SetSummary(ctx, "u1", "c1", want))

	got, err := store.GetSummary(ctx, "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != want.Text || got.CoversThrough != want.CoversThrough {
		t.Errorf("GetSummary() = %+v, want %+v", got, want)
	}
}

func TestInMemoryStore_SummaryIsolatesByUserAndConversation(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	must(t, store.SetSummary(ctx, "u1", "c1", Summary{Text: "u1/c1 summary"}))
	must(t, store.SetSummary(ctx, "u2", "c1", Summary{Text: "u2/c1 summary"}))

	got, err := store.GetSummary(ctx, "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "u1/c1 summary" {
		t.Errorf("GetSummary(u1, c1) = %+v, want Text=%q", got, "u1/c1 summary")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
