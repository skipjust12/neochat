//go:build integration

package settings

import (
	"context"
	"encoding/json"
	"testing"

	"neochat/internal/dbtest"
)

func TestPostgresStore(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "user_settings")
	s := NewPostgresStore(pgDB)
	ctx := context.Background()

	if got, err := s.Get(ctx, "u1"); err != nil || got != nil {
		t.Fatalf("Get before Put = %s, %v", got, err)
	}
	for _, value := range []string{`{"theme":"dark"}`, `{"theme":"light","tone":"Direct"}`} {
		if err := s.Put(ctx, "u1", json.RawMessage(value)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, "u1")
	var decoded map[string]string
	if err != nil || json.Unmarshal(got, &decoded) != nil || decoded["theme"] != "light" || decoded["tone"] != "Direct" {
		t.Fatalf("Get = %s, %v", got, err)
	}
	if other, _ := s.Get(ctx, "u2"); other != nil {
		t.Fatalf("another user got %s", other)
	}
}
