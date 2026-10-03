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

func (s *PostgresStore) Sweep(ctx context.Context, cutoff time.Time) (int64, error) {
	stale, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE conversation_id = '' AND created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	// conversation_messages is the conversation package's table, but it is
	// the only record of which chats exist; a claimed file whose chat has no
	// messages left belongs to a deleted chat. No age condition: a file is
	// only claimed after its message is stored, so a fresh claim always has
	// messages behind it.
	orphans, err := s.db.ExecContext(ctx, `
		DELETE FROM attachments a
		WHERE a.conversation_id <> ''
		  AND NOT EXISTS (
			SELECT 1 FROM conversation_messages m
			WHERE m.user_id = a.user_id AND m.conversation_id = a.conversation_id
		  )
	`)
	if err != nil {
		return 0, err
	}
	n1, _ := stale.RowsAffected()
	n2, _ := orphans.RowsAffected()
	return n1 + n2, nil
}
