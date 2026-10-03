package pagestore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PostgresStore keeps pages in the web_pages table (see
// db/migrations/0009_web_pages.sql).
type PostgresStore struct {
	db *sql.DB
}

func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Get(ctx context.Context, userID, conversationID, url string, notBefore time.Time) (Page, bool, error) {
	p := Page{UserID: userID, ConversationID: conversationID, URL: url}
	err := s.db.QueryRowContext(ctx, `
		SELECT final_url, title, content, truncated, fetched_at
		FROM web_pages
		WHERE user_id = $1 AND conversation_id = $2 AND url = $3 AND fetched_at >= $4
	`, userID, conversationID, url, notBefore).Scan(&p.FinalURL, &p.Title, &p.Text, &p.Truncated, &p.FetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Page{}, false, nil
	}
	if err != nil {
		return Page{}, false, err
	}
	return p, true, nil
}

func (s *PostgresStore) Put(ctx context.Context, p Page) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO web_pages (user_id, conversation_id, url, final_url, title, content, content_bytes, truncated, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (user_id, conversation_id, url) DO UPDATE SET
			final_url = EXCLUDED.final_url, title = EXCLUDED.title, content = EXCLUDED.content,
			content_bytes = EXCLUDED.content_bytes, truncated = EXCLUDED.truncated, fetched_at = EXCLUDED.fetched_at
	`, p.UserID, p.ConversationID, p.URL, p.FinalURL, p.Title, p.Text, p.Bytes(), p.Truncated, p.FetchedAt)
	return err
}

func (s *PostgresStore) DeleteConversation(ctx context.Context, userID, conversationID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_pages WHERE user_id = $1 AND conversation_id = $2`, userID, conversationID)
	return err
}

func (s *PostgresStore) Sweep(ctx context.Context, expiredBefore, orphanedBefore time.Time, maxBytes int64) (int64, error) {
	var total int64
	expired, err := s.db.ExecContext(ctx, `DELETE FROM web_pages WHERE fetched_at < $1`, expiredBefore)
	if err != nil {
		return 0, err
	}
	n, _ := expired.RowsAffected()
	total += n
	// conversation_messages belongs to package conversation, but it is the
	// only record of which chats exist (same rule as the attachments sweep).
	orphans, err := s.db.ExecContext(ctx, `
		DELETE FROM web_pages p
		WHERE p.fetched_at < $1
		  AND NOT EXISTS (
			SELECT 1 FROM conversation_messages m
			WHERE m.user_id = p.user_id AND m.conversation_id = p.conversation_id
		  )
	`, orphanedBefore)
	if err != nil {
		return total, err
	}
	n, _ = orphans.RowsAffected()
	total += n
	overBudget, err := s.db.ExecContext(ctx, `
		DELETE FROM web_pages w
		USING (
			SELECT user_id, conversation_id, url,
			       SUM(content_bytes) OVER (ORDER BY fetched_at DESC, user_id, conversation_id, url) AS running
			FROM web_pages
		) ranked
		WHERE ranked.running > $1
		  AND w.user_id = ranked.user_id AND w.conversation_id = ranked.conversation_id AND w.url = ranked.url
	`, maxBytes)
	if err != nil {
		return total, err
	}
	n, _ = overBudget.RowsAffected()
	return total + n, nil
}
