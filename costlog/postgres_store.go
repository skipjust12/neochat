package costlog

import (
	"context"
	"database/sql"

	"neochat/router"
)

// PostgresStore is a Store backed by the cost_log table
// (db/migrations/0003_cost_log.sql) -- billing data, append-only.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by db. Callers are responsible
// for migrating db first (see db.Connect, which does this automatically).
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Record(ctx context.Context, entry router.CostLogEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cost_log (user_id, request_id, model_id, mode, input_tokens, output_tokens, cost_usd, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, entry.UserID, entry.RequestID, entry.ModelID, entry.Mode, entry.InputTokens, entry.OutputTokens, entry.CostUSD, entry.Timestamp)
	return err
}

// entriesForTest returns every recorded entry, oldest first. Not part of
// the Store interface -- mirrors InMemoryStore.Entries() closely enough
// to reuse the same test assertions against a real database.
func (s *PostgresStore) entriesForTest(ctx context.Context) ([]router.CostLogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, request_id, model_id, mode, input_tokens, output_tokens, cost_usd, created_at
		FROM cost_log
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []router.CostLogEntry
	for rows.Next() {
		var e router.CostLogEntry
		if err := rows.Scan(&e.UserID, &e.RequestID, &e.ModelID, &e.Mode, &e.InputTokens, &e.OutputTokens, &e.CostUSD, &e.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
