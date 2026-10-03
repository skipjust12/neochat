package attachment

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PostgresStore keeps uploads in the attachments table (see
// db/migrations/0008_attachments.sql). File bytes live in a BYTEA column:
// at a 20 MB ceiling per file that is simpler to operate than a separate
// object store, and it rides the existing Postgres backups.
type PostgresStore struct {
	db *sql.DB
}

func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Put(ctx context.Context, f File) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO attachments (user_id, attachment_id, conversation_id, name, mime, kind, size, data, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, f.UserID, f.ID, f.ConversationID, f.Name, f.MIME, f.Kind, f.Size, f.Data, f.CreatedAt)
	return err
}

func (s *PostgresStore) Get(ctx context.Context, userID, id string) (File, error) {
	f := File{UserID: userID, ID: id}
	err := s.db.QueryRowContext(ctx, `
		SELECT conversation_id, name, mime, kind, size, data, created_at
		FROM attachments WHERE user_id = $1 AND attachment_id = $2
	`, userID, id).Scan(&f.ConversationID, &f.Name, &f.MIME, &f.Kind, &f.Size, &f.Data, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	return f, err
}

func (s *PostgresStore) Claim(ctx context.Context, userID, conversationID string, ids []string) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE attachments SET conversation_id = $3
			WHERE user_id = $1 AND attachment_id = $2 AND conversation_id = ''
		`, userID, id, conversationID); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) DeleteConversation(ctx context.Context, userID, conversationID string) error {
	if conversationID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE user_id = $1 AND conversation_id = $2`, userID, conversationID)
	return err
}

func (s *PostgresStore) PruneUnclaimed(ctx context.Context, userID string, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM attachments WHERE user_id = $1 AND conversation_id = '' AND created_at < $2
	`, userID, cutoff)
	return err
}

func (s *PostgresStore) CountUnclaimed(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM attachments WHERE user_id = $1 AND conversation_id = ''
	`, userID).Scan(&n)
	return n, err
}
