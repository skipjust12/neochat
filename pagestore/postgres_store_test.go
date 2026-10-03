//go:build integration

package pagestore

import (
	"context"
	"strings"
	"testing"
	"time"

	"neochat/internal/dbtest"
)

func TestPostgresStore(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "web_pages", "conversation_messages")
	s := NewPostgresStore(pgDB)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	page := Page{UserID: "u1", ConversationID: "c1", URL: "https://a.dev", FinalURL: "https://a.dev/", Title: "A", Text: "привет", Truncated: true, FetchedAt: now}
	if err := s.Put(ctx, page); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "u1", "c1", "https://a.dev", now.Add(-time.Hour))
	if err != nil || !ok || got.Text != "привет" || got.Title != "A" || got.FinalURL != "https://a.dev/" || !got.Truncated || !got.FetchedAt.Equal(now) {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := s.Get(ctx, "u2", "c1", "https://a.dev", time.Time{}); ok {
		t.Fatal("another user read the page")
	}
	if _, ok, _ := s.Get(ctx, "u1", "c1", "https://a.dev", now.Add(time.Second)); ok {
		t.Fatal("a page older than notBefore was returned")
	}
	page.Text, page.FetchedAt = "updated", now.Add(time.Second)
	if err := s.Put(ctx, page); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Get(ctx, "u1", "c1", "https://a.dev", time.Time{}); got.Text != "updated" {
		t.Fatalf("Put didn't replace the page: %q", got.Text)
	}
	if err := s.DeleteConversation(ctx, "u1", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "u1", "c1", "https://a.dev", time.Time{}); ok {
		t.Fatal("DeleteConversation kept the page")
	}
}

func TestPostgresStoreSweep(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "web_pages", "conversation_messages")
	s := NewPostgresStore(pgDB)
	ctx := context.Background()
	now := time.Now().UTC()
	// c1 is a stored chat; c2 never got a message (stopped, or deleted
	// while the store was down); c3 is a chat still being answered.
	if _, err := pgDB.ExecContext(ctx, `
		INSERT INTO conversation_messages (user_id, conversation_id, role, content, created_at)
		VALUES ('u1', 'c1', 'user', 'hi', now())`); err != nil {
		t.Fatal(err)
	}
	put := func(conv, url string, age time.Duration, size int) {
		t.Helper()
		if err := s.Put(ctx, Page{UserID: "u1", ConversationID: conv, URL: url, FinalURL: url, Text: strings.Repeat("x", size), FetchedAt: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	put("c1", "expired", 25*time.Hour, 10)
	put("c1", "old", 5*time.Hour, 1000)
	put("c1", "mid", 4*time.Hour, 1000)
	put("c1", "new", 3*time.Hour, 1000)
	put("c2", "orphan", 2*time.Hour, 10)
	put("c3", "in-flight", time.Minute, 10)

	// Each kept page counts its text plus both URLs.
	budget := Page{URL: "new", FinalURL: "new", Text: strings.Repeat("x", 1000)}.Bytes()*2 + Page{URL: "in-flight", FinalURL: "in-flight", Text: "xxxxxxxxxx"}.Bytes()
	deleted, err := s.Sweep(ctx, now.Add(-TTL), now.Add(-OrphanGrace), budget)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Errorf("Sweep deleted %d, want expired, orphan and the oldest over budget", deleted)
	}
	for _, c := range []struct {
		conv, url string
		kept      bool
	}{{"c1", "expired", false}, {"c1", "old", false}, {"c1", "mid", true}, {"c1", "new", true}, {"c2", "orphan", false}, {"c3", "in-flight", true}} {
		if _, ok, _ := s.Get(ctx, "u1", c.conv, c.url, time.Time{}); ok != c.kept {
			t.Errorf("%s/%s kept = %v, want %v", c.conv, c.url, ok, c.kept)
		}
	}
}
