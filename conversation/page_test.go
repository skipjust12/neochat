package conversation

import (
	"context"
	"strings"
	"testing"
)

func exercisePages(t *testing.T, store Store) {
	ctx := context.Background()
	user := NewID()
	conversationID := NewID()
	for i := 0; i < 45; i++ {
		if err := store.Append(ctx, user, conversationID, Message{Role: RoleUser, Content: "message"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.HistoryPage(ctx, user, conversationID, 0, 20)
	if err != nil || len(first.Messages) != 20 || first.NextCursor == 0 {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := store.HistoryPage(ctx, user, conversationID, first.NextCursor, 20)
	if err != nil || len(second.Messages) != 20 || second.Messages[19].ID >= first.Messages[0].ID {
		t.Fatalf("second page overlaps: %+v %v", second, err)
	}
	third, err := store.HistoryPage(ctx, user, conversationID, second.NextCursor, 20)
	if err != nil || len(third.Messages) != 5 || third.NextCursor != 0 {
		t.Fatalf("last page: %+v %v", third, err)
	}
	hidden, err := store.HistoryPage(ctx, "other", conversationID, 0, 20)
	if err != nil || len(hidden.Messages) != 0 {
		t.Fatalf("cross-user disclosure: %+v %v", hidden, err)
	}
	if _, err := store.HistoryPage(ctx, user, conversationID, 0, MaxPageSize+1); err == nil {
		t.Fatal("unbounded page accepted")
	}
	largeID := NewID()
	if err := store.Append(ctx, user, largeID, Message{Role: RoleUser, Content: strings.Repeat("я", maxMessageRunes+1)}); err != nil {
		t.Fatal(err)
	}
	bounded, err := store.HistoryPage(ctx, user, largeID, 0, 20)
	if err != nil || !bounded.Messages[0].Truncated || len([]rune(bounded.Messages[0].Content)) != maxMessageRunes {
		t.Fatal("legacy oversized message not bounded")
	}
}
func TestMemoryHistoryPagination(t *testing.T) { exercisePages(t, NewInMemoryStore()) }
