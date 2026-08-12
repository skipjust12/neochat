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

// Migrate applies every embedded .sql file under migrations/ that hasn't
// already been recorded in schema_migrations, in filename order, each
// inside its own transaction. Safe to call on every cmd/server startup --
// already-applied files are skipped, so there is no separate migration
// step to remember on a solo dev box.
func Migrate(ctx context.Context, pgDB *sql.DB) error {
	if _, err := pgDB.ExecContext(ctx, `
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

	for _, name := range names {
		var already bool
		if err := pgDB.QueryRowContext(ctx,
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

		tx, err := pgDB.BeginTx(ctx, nil)
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
