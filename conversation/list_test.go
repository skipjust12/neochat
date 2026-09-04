package conversation

import (
	"context"
	"testing"
	"time"
)

func TestInMemoryStoreList(t *testing.T) {
	store := NewInMemoryStore()
	older := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	if err := store.Append(context.Background(), "user-a", "older", Message{Role: RoleUser, Content: "  First conversation  ", CreatedAt: older}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), "user-a", "newer", Message{Role: RoleUser, Content: "Newest conversation", CreatedAt: newer}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), "user-b", "hidden", Message{Role: RoleUser, Content: "Other user's chat", CreatedAt: newer.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	got, err := store.List(context.Background(), "user-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d conversations, want 2", len(got))
	}
	if got[0].ID != "newer" || got[0].Title != "Newest conversation" {
		t.Fatalf("first conversation = %#v", got[0])
	}
	if got[1].ID != "older" || got[1].Title != "First conversation" {
		t.Fatalf("second conversation = %#v", got[1])
	}
}
