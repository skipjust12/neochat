// Package dbtest provides shared setup for the *_test.go files under the
// //go:build integration tag across limits/moderation/conversation/
// idempotency/costlog -- connecting to the real docker-compose Postgres/
// Redis instead of an InMemory* stand-in. See docs/running-locally.md for
// how to run these (docker-compose up -d, then
// `set -a && source .env && set +a && go test -tags=integration ./...`).
package dbtest

import (
	"context"
	"os"
	"testing"

	"database/sql"

	"github.com/redis/go-redis/v9"

	"neochat/db"
)

// Postgres returns a migrated connection to the docker-compose Postgres
// instance, or skips the test if POSTGRES_USER isn't set (i.e. the .env
// vars this package needs weren't exported into the test process).
func Postgres(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("POSTGRES_USER") == "" {
		t.Skip("POSTGRES_* env vars not set -- run `docker-compose up -d` and export .env first, see docs/running-locally.md")
	}
	pgDB, err := db.Connect(context.Background())
	if err != nil {
		t.Fatalf("dbtest.Postgres: %v", err)
	}
	t.Cleanup(func() { pgDB.Close() })
	return pgDB
}

// Redis returns a connection to the docker-compose Redis instance, or
// skips the test if REDIS_PASSWORD isn't set. FLUSHDB is not called here
// automatically -- callers key their test data so it doesn't collide, or
// call FlushRedis explicitly (see idempotency/redis_store_test.go and
// limits/redis_store_test.go).
func Redis(t *testing.T) *redis.Client {
	t.Helper()
	if os.Getenv("REDIS_PASSWORD") == "" {
		t.Skip("REDIS_* env vars not set -- run `docker-compose up -d` and export .env first, see docs/running-locally.md")
	}
	rdb, err := db.ConnectRedis(context.Background())
	if err != nil {
		t.Fatalf("dbtest.Redis: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// TruncateTables clears every listed table (RESTART IDENTITY so BIGSERIAL
// ids reset too) -- called at the start of a test, not the end, so a
// failed previous run doesn't leave stale rows behind to hide a real bug.
func TruncateTables(t *testing.T, pgDB *sql.DB, tables ...string) {
	t.Helper()
	for _, table := range tables {
		if _, err := pgDB.ExecContext(context.Background(), "TRUNCATE TABLE "+table+" RESTART IDENTITY"); err != nil {
			t.Fatalf("dbtest.TruncateTables(%s): %v", table, err)
		}
	}
}

// FlushRedis deletes every key under the given prefixes -- called at the
// start of a test for the same "clean slate, but don't hide a leftover
// bug from a prior failed run" reasoning as TruncateTables. Not a blanket
// FLUSHDB, since limits/ and idempotency/ share the same Redis instance
// and a test for one shouldn't wipe the other's keys mid-suite.
func FlushRedis(t *testing.T, rdb *redis.Client, prefixes ...string) {
	t.Helper()
	ctx := context.Background()
	for _, prefix := range prefixes {
		iter := rdb.Scan(ctx, 0, prefix+"*", 0).Iterator()
		var keys []string
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		if err := iter.Err(); err != nil {
			t.Fatalf("dbtest.FlushRedis(%s): scan: %v", prefix, err)
		}
		if len(keys) > 0 {
			if err := rdb.Del(ctx, keys...).Err(); err != nil {
				t.Fatalf("dbtest.FlushRedis(%s): del: %v", prefix, err)
			}
		}
	}
}
