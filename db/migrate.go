package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"sort"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"

// migrationAdvisoryLockID is the Postgres advisory lock every Migrate
// call serializes on -- an arbitrary but fixed application-wide constant
// ("neochat" as ASCII hex), chosen only so two processes agree on it.
const migrationAdvisoryLockID int64 = 0x6E656F63686174

// Migrate applies every embedded .sql file under migrations/ that hasn't
// already been recorded in schema_migrations, in filename order, each
// inside its own transaction. Safe to call on every cmd/server startup --
// already-applied files are skipped, so there is no separate migration
// step to remember on a solo dev box.
//
// Concurrency-safe across processes: the whole run is serialized behind a
// Postgres advisory lock, so two instances starting at once (the
// horizontally-scaled deployment README's "stateless app layer" item is
// aiming at) cannot both decide a migration is unapplied and both run it.
// Without the lock they interleave between the check and the INSERT
// below, and the loser dies on schema_migrations' primary key -- the
// server simply failing to start -- while any migration doing more than
// CREATE TABLE IF NOT EXISTS would have been applied twice first. The
// second process blocks here rather than erroring, then finds every
// migration already recorded and proceeds normally.
func Migrate(ctx context.Context, pgDB *sql.DB) error {
	// A dedicated connection, because an advisory lock belongs to the
	// session that took it -- taking it on a pooled *sql.DB would leave
	// which session holds it up to chance.
	conn, err := pgDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db: acquire connection for migration lock: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationAdvisoryLockID); err != nil {
		return fmt.Errorf("db: acquire migration lock: %w", err)
	}
	defer func() {
		// WithoutCancel so an already-canceled ctx still releases the lock:
		// conn.Close only returns the connection to the pool, it does not
		// end the session, and a session-scoped advisory lock outlives the
		// pooled connection's current user. (If this Exec fails at all, it
		// failed because the connection itself is broken, which
		// database/sql handles by discarding it -- ending the session and
		// releasing the lock that way instead.)
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationAdvisoryLockID); err != nil {
			log.Printf("db: failed to release migration lock: %v", err)
		}
	}()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("db: create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		return fmt.Errorf("db: read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	// Everything below runs on conn -- the same session holding the
	// advisory lock -- rather than on the pool, so the check/apply/record
	// sequence can't be interleaved with another process's.
	for _, name := range names {
		var already bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`, name,
		).Scan(&already); err != nil {
			return fmt.Errorf("db: check migration %s: %w", name, err)
		}
		if already {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile(migrationsDir + "/" + name)
		if err != nil {
			return fmt.Errorf("db: read migration %s: %w", name, err)
		}

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("db: begin tx for %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("db: apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (filename) VALUES ($1)`, name,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("db: record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("db: commit migration %s: %w", name, err)
		}
		log.Printf("db: applied migration %s", name)
	}

	return nil
}
