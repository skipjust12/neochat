package conversation

import (
	"context"
	"database/sql"
	"errors"
)

// PostgresStore is a Store backed by the conversation_messages/
// conversation_summaries tables (db/migrations/0002_conversation.sql).
// See Store's doc comment for the swap-in procedure this follows --
// message-per-row keyed on (user_id, conversation_id), ordered by
// insertion (id).
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by db. Callers are responsible
// for migrating db first (see db.Connect, which does this automatically).
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Append(ctx context.Context, userID, conversationID string, msg Message) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversation_messages (user_id, conversation_id, role, content, model_id, is_summary, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, userID, conversationID, msg.Role, msg.Content, msg.ModelID, msg.IsSummary, msg.CreatedAt)
	return err
}

func (s *PostgresStore) History(ctx context.Context, userID, conversationID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, model_id, is_summary, created_at
		FROM conversation_messages
		WHERE user_id = $1 AND conversation_id = $2
		ORDER BY id
	`, userID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Not nil for an unknown/empty conversation -- matches InMemoryStore's
	// "empty slice, not an error, and not a nil slice a caller might treat
	// differently" contract.
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Role, &m.Content, &m.ModelID, &m.IsSummary, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetSummary(ctx context.Context, userID, conversationID string) (Summary, error) {
	var sum Summary
	err := s.db.QueryRowContext(ctx, `
		SELECT text, covers_through, updated_at
		FROM conversation_summaries
		WHERE user_id = $1 AND conversation_id = $2
	`, userID, conversationID).Scan(&sum.Text, &sum.CoversThrough, &sum.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Summary{}, nil
	}
	if err != nil {
		return Summary{}, err
	}
	return sum, nil
}

func (s *PostgresStore) SetSummary(ctx context.Context, userID, conversationID string, summary Summary) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversation_summaries (user_id, conversation_id, text, covers_through, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, conversation_id)
		DO UPDATE SET text = $3, covers_through = $4, updated_at = $5
	`, userID, conversationID, summary.Text, summary.CoversThrough, summary.UpdatedAt)
	return err
}
