//go:build integration

package attachment

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"neochat/internal/dbtest"
)

func TestPostgresStoreLifecycle(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "attachments")
	s := NewPostgresStore(pgDB)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	file := File{ID: "f1", UserID: "u1", Name: "a.png", MIME: "image/png", Kind: KindImage, Size: 3, Data: []byte{1, 2, 3}, CreatedAt: now}
	if err := s.Put(ctx, file); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, File{ID: "old", UserID: "u1", Name: "o.txt", MIME: "text/plain", Kind: KindText, Size: 1, Data: []byte("o"), CreatedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "u1", "f1")
	if err != nil || got.Name != "a.png" || !bytes.Equal(got.Data, file.Data) || got.ConversationID != "" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, "u2", "f1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user read the file: %v", err)
	}
	if n, err := s.CountUnclaimed(ctx, "u1"); err != nil || n != 2 {
		t.Fatalf("CountUnclaimed = %d, %v", n, err)
	}
	if err := s.Claim(ctx, "u1", "c1", []string{"f1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(ctx, "u1", "c2", []string{"f1"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "u1", "f1"); got.ConversationID != "c1" {
		t.Fatalf("claimed file moved to %q", got.ConversationID)
	}
	if err := s.PruneUnclaimed(ctx, "u1", now.Add(-UnclaimedTTL)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "u1", "old"); !errors.Is(err, ErrNotFound) {
		t.Fatal("stale unclaimed file survived")
	}
	if err := s.DeleteConversation(ctx, "u1", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "u1", "f1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("conversation delete kept its file")
	}
}
