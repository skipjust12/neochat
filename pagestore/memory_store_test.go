package pagestore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestInMemoryStore(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	now := time.Now()
	page := Page{UserID: "u1", ConversationID: "c1", URL: "https://a.dev", FinalURL: "https://a.dev/", Title: "A", Text: "hello", FetchedAt: now}
	if err := s.Put(ctx, page); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.Get(ctx, "u1", "c1", "https://a.dev", now.Add(-time.Hour)); !ok || got.Text != "hello" {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	for _, miss := range [][3]string{{"u2", "c1", "https://a.dev"}, {"u1", "c2", "https://a.dev"}, {"u1", "c1", "https://b.dev"}} {
		if _, ok, _ := s.Get(ctx, miss[0], miss[1], miss[2], time.Time{}); ok {
			t.Fatalf("Get%v found another chat's page", miss)
		}
	}
	if _, ok, _ := s.Get(ctx, "u1", "c1", "https://a.dev", now.Add(time.Minute)); ok {
		t.Fatal("a page older than notBefore was returned")
	}

	if err := s.DeleteConversation(ctx, "u1", "c1"); err != nil || s.Len() != 0 {
		t.Fatalf("DeleteConversation left %d pages, %v", s.Len(), err)
	}
}

func TestInMemoryStoreSweep(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	now := time.Now()
	put := func(url string, age time.Duration, size int) {
		t.Helper()
		if err := s.Put(ctx, Page{UserID: "u1", ConversationID: "c1", URL: url, Text: strings.Repeat("x", size), FetchedAt: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	put("expired", 25*time.Hour, 10)
	put("old", 3*time.Hour, 1000)
	put("mid", 2*time.Hour, 1000)
	put("new", time.Hour, 1000)

	deleted, err := s.Sweep(ctx, now.Add(-TTL), now.Add(-OrphanGrace), 2100)
	if err != nil || deleted != 2 {
		t.Fatalf("Sweep deleted %d, %v; want the expired page and the oldest over budget", deleted, err)
	}
	for url, want := range map[string]bool{"expired": false, "old": false, "mid": true, "new": true} {
		if _, ok, _ := s.Get(ctx, "u1", "c1", url, time.Time{}); ok != want {
			t.Errorf("%s kept = %v, want %v", url, ok, want)
		}
	}
}
