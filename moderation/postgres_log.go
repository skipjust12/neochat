package moderation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// PostgresBlockLog is a BlockLog backed by the moderation_block_log table
// (db/migrations/0001_moderation_block_log.sql). See BlockLog's doc
// comment for the swap-in procedure this follows. Categories is stored as
// JSONB rather than a native Postgres array -- keeps this package on
// plain database/sql + encoding/json with no extra driver-specific
// dependency beyond pgx itself.
type PostgresBlockLog struct {
	db *sql.DB
}

// NewPostgresBlockLog returns a BlockLog backed by db. Callers are
// responsible for migrating db first (see db.Connect, which does this
// automatically).
func NewPostgresBlockLog(db *sql.DB) *PostgresBlockLog {
	return &PostgresBlockLog{db: db}
}

func (l *PostgresBlockLog) Record(ctx context.Context, entry BlockEntry) error {
	categories, err := json.Marshal(entry.Categories)
	if err != nil {
		return fmt.Errorf("moderation: marshal categories: %w", err)
	}
	_, err = l.db.ExecContext(ctx, `
		INSERT INTO moderation_block_log (user_id, categories, reason, created_at)
		VALUES ($1, $2, $3, $4)
	`, entry.UserID, categories, entry.Reason, entry.Timestamp)
	return err
}

// entriesForTest returns every recorded entry, oldest first. Not part of
// the BlockLog interface -- a real caller would query for a purpose
// (rate/category dashboards, not a full dump) -- but it mirrors
// InMemoryBlockLog.Entries() closely enough to reuse the same test
// assertions against a real database.
func (l *PostgresBlockLog) entriesForTest(ctx context.Context) ([]BlockEntry, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT user_id, categories, reason, created_at
		FROM moderation_block_log
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BlockEntry
	for rows.Next() {
		var e BlockEntry
		var categories []byte
		if err := rows.Scan(&e.UserID, &categories, &e.Reason, &e.Timestamp); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(categories, &e.Categories); err != nil {
			return nil, fmt.Errorf("moderation: unmarshal categories: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
