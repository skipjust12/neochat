//go:build integration

package db

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
)

// testPostgres connects to the docker-compose Postgres the same way
// internal/dbtest does for every other package's integration tests. It is
// inlined here rather than reused from dbtest because dbtest imports this
// package -- an in-package test importing it back would be an import
// cycle. See docs/running-locally.md for how to run these.
func testPostgres(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("POSTGRES_USER") == "" {
		t.Skip("POSTGRES_* env vars not set -- run `docker-compose up -d` and export .env first, see docs/running-locally.md")
	}
	pgDB, err := Connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pgDB.Close() })
	return pgDB
}

// TestMigrate_ConcurrentCallsAreSerialized is the regression test for
// audit.md's migration-race finding: several processes starting at once
// must not both try to apply the same migration. Without the advisory
// lock, the losers of the race fail on schema_migrations' primary key
// ("duplicate key value violates unique constraint") and the server they
// belong to never starts.
//
// schema_migrations is dropped first so every caller genuinely sees an
// unmigrated database and races to apply; the migrations themselves are
// CREATE TABLE IF NOT EXISTS, so re-running them against the already-
// created tables is harmless.
func TestMigrate_ConcurrentCallsAreSerialized(t *testing.T) {
	pgDB := testPostgres(t)
	ctx := context.Background()

	if _, err := pgDB.ExecContext(ctx, `DROP TABLE IF EXISTS schema_migrations`); err != nil {
		t.Fatalf("drop schema_migrations: %v", err)
	}

	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = Migrate(ctx, pgDB)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate call %d failed: %v", i, err)
		}
	}

	// Each migration file must be recorded exactly once -- the primary key
	// makes duplicates impossible to store, so a count below the number of
	// files would mean one was skipped, and a double-apply would have
	// surfaced as an error above.
	var recorded int
	if err := pgDB.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	entries, err := migrationsFS.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	if recorded != len(entries) {
		t.Errorf("schema_migrations has %d rows, want %d (one per embedded migration)", recorded, len(entries))
	}
}

// TestMigrate_ReleasesAdvisoryLock checks the lock does not outlive the
// call. A session-scoped advisory lock survives conn.Close (which only
// returns the connection to the pool), so a missing unlock would wedge
// every later Migrate -- including the next process to boot -- rather
// than showing up as a failure here.
func TestMigrate_ReleasesAdvisoryLock(t *testing.T) {
	pgDB := testPostgres(t)
	ctx := context.Background()

	if err := Migrate(ctx, pgDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var held bool
	if err := pgDB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND ((classid::bigint << 32) | objid::bigint) = $1)`,
		migrationAdvisoryLockID,
	).Scan(&held); err != nil {
		t.Fatalf("query pg_locks: %v", err)
	}
	if held {
		t.Error("migration advisory lock is still held after Migrate returned")
	}
}
