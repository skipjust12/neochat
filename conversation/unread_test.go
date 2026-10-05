package conversation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInMemoryStore_UnreadUntilMarkedRead(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	project := "p1"

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	unreadOf := func(list []Overview, id string) bool {
		t.Helper()
		for _, item := range list {
			if item.ID == id {
				return item.Unread
			}
		}
		t.Fatalf("%s missing from %+v", id, list)
		return false
	}
	list := func() []Overview {
		t.Helper()
		out, err := store.List(ctx, "u1", 10)
		must(err)
		return out
	}

	// A question with no reply yet is not unread.
	must(store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "hi", CreatedAt: t0}))
	if unreadOf(list(), "c1") {
		t.Fatal("a chat without a reply is unread")
	}
	// The reply makes it unread until it is opened.
	must(store.Append(ctx, "u1", "c1", Message{Role: RoleAssistant, Content: "hello", CreatedAt: t0.Add(time.Second)}))
	if !unreadOf(list(), "c1") {
		t.Fatal("a chat with a reply never opened is not unread")
	}
	must(store.MarkRead(ctx, "u1", "c1", t0.Add(2*time.Second)))
	if unreadOf(list(), "c1") {
		t.Fatal("still unread after MarkRead")
	}
	// An older MarkRead (a slow request arriving late) doesn't undo it.
	must(store.MarkRead(ctx, "u1", "c1", t0))
	if unreadOf(list(), "c1") {
		t.Fatal("an older MarkRead moved the read time back")
	}
	// A regenerated reply is new again.
	_, err := store.AddResponseVersion(ctx, "u1", "c1", 2, ResponseVersion{Content: "hello again", CreatedAt: t0.Add(3 * time.Second)})
	must(err)
	if !unreadOf(list(), "c1") {
		t.Fatal("a regenerated reply isn't unread")
	}
	// A compact summary is not a reply.
	must(store.MarkRead(ctx, "u1", "c1", t0.Add(4*time.Second)))
	must(store.Append(ctx, "u1", "c1", Message{Role: RoleAssistant, Content: "summary", IsSummary: true, CreatedAt: t0.Add(5 * time.Second)}))
	if unreadOf(list(), "c1") {
		t.Fatal("a summary marked the chat unread")
	}

	// Project chats report it too.
	must(store.Append(ctx, "u1", "c2", Message{Role: RoleUser, Content: "q", CreatedAt: t0}))
	must(store.Append(ctx, "u1", "c2", Message{Role: RoleAssistant, Content: "a", CreatedAt: t0.Add(time.Second)}))
	must(store.UpdateMetadata(ctx, "u1", "c2", MetadataUpdate{ProjectID: &project}))
	chats, err := store.ListByProject(ctx, "u1", project, 10)
	must(err)
	if !unreadOf(chats, "c2") {
		t.Fatal("an unread project chat isn't unread")
	}

	if err := store.MarkRead(ctx, "u1", "missing", t0); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("MarkRead(missing) = %v, want ErrConversationNotFound", err)
	}
	if err := store.MarkRead(ctx, "u2", "c1", t0); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("MarkRead(another user's chat) = %v, want ErrConversationNotFound", err)
	}
}
