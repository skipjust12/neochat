package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
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
	if msg.Role == RoleAssistant && len(msg.Versions) == 0 {
		msg.Versions = []ResponseVersion{{Content: msg.Content, ModelID: msg.ModelID, CreatedAt: msg.CreatedAt}}
	}
	versions, err := json.Marshal(msg.Versions)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO conversation_messages (user_id, conversation_id, role, content, model_id, is_summary, created_at, versions)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, userID, conversationID, msg.Role, msg.Content, msg.ModelID, msg.IsSummary, msg.CreatedAt, versions)
	return err
}

func (s *PostgresStore) AddResponseVersion(ctx context.Context, userID, conversationID string, messageID int64, version ResponseVersion) (Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()

	var message Message
	var encoded []byte
	err = tx.QueryRowContext(ctx, `
		SELECT id, role, content, model_id, is_summary, created_at, versions
		FROM conversation_messages
		WHERE user_id = $1 AND conversation_id = $2 AND id = $3 AND role = $4
		FOR UPDATE
	`, userID, conversationID, messageID, RoleAssistant).Scan(
		&message.ID, &message.Role, &message.Content, &message.ModelID, &message.IsSummary, &message.CreatedAt, &encoded,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrMessageNotFound
	}
	if err != nil {
		return Message{}, err
	}
	if len(encoded) > 0 {
		if err := json.Unmarshal(encoded, &message.Versions); err != nil {
			return Message{}, err
		}
	}
	if len(message.Versions) == 0 {
		message.Versions = []ResponseVersion{{Content: message.Content, ModelID: message.ModelID, CreatedAt: message.CreatedAt}}
	}
	if len(message.Versions)-1 >= MaxRegenerationAttempts {
		return Message{}, ErrRegenerationLimit
	}
	message.Versions = append(message.Versions, version)
	message.Content = version.Content
	message.ModelID = version.ModelID
	message.CreatedAt = version.CreatedAt
	encoded, err = json.Marshal(message.Versions)
	if err != nil {
		return Message{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversation_messages
		SET content = $1, model_id = $2, created_at = $3, versions = $4
		WHERE user_id = $5 AND conversation_id = $6 AND id = $7
	`, message.Content, message.ModelID, message.CreatedAt, encoded, userID, conversationID, messageID); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}
	return message, nil
}

func (s *PostgresStore) History(ctx context.Context, userID, conversationID string, offset int) ([]Message, error) {
	if offset < 0 {
		offset = 0
	}
	// OFFSET (not a "newest N" LIMIT) because the caller's offset is an
	// index into this exact ordering -- see Store.History's doc comment on
	// why that correspondence has to hold for Summary.CoversThrough to
	// stay meaningful. The (user_id, conversation_id, id) index in
	// db/migrations/0002_conversation.sql covers this scan.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, role, content, model_id, is_summary, created_at, versions
		FROM conversation_messages
		WHERE user_id = $1 AND conversation_id = $2
		ORDER BY id
		OFFSET $3
	`, userID, conversationID, offset)
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
		var encoded []byte
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.ModelID, &m.IsSummary, &m.CreatedAt, &encoded); err != nil {
			return nil, err
		}
		if len(encoded) > 0 {
			if err := json.Unmarshal(encoded, &m.Versions); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) List(ctx context.Context, userID string, limit int) ([]Overview, error) {
	if limit <= 0 {
		return []Overview{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT grouped.conversation_id,
		       COALESCE(NULLIF(metadata.title, ''), NULLIF((
		           SELECT LEFT(BTRIM(first_message.content), 80)
		           FROM conversation_messages first_message
		           WHERE first_message.user_id = $1
		             AND first_message.conversation_id = grouped.conversation_id
		             AND first_message.role = $3
		           ORDER BY first_message.id
		           LIMIT 1
		       ), ''), 'New chat') AS title,
		       COALESCE(metadata.pinned, FALSE),
		       grouped.updated_at
		FROM (
			SELECT conversation_id, MAX(created_at) AS updated_at
			FROM conversation_messages
			WHERE user_id = $1
			GROUP BY conversation_id
		) grouped
		LEFT JOIN conversation_metadata metadata
		  ON metadata.user_id = $1 AND metadata.conversation_id = grouped.conversation_id
		WHERE COALESCE(metadata.project_id, '') = ''
		ORDER BY COALESCE(metadata.pinned, FALSE) DESC, grouped.updated_at DESC
		LIMIT $2
	`, userID, limit, RoleUser)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Overview{}
	for rows.Next() {
		var overview Overview
		if err := rows.Scan(&overview.ID, &overview.Title, &overview.Pinned, &overview.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, overview)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListByProject(ctx context.Context, userID, projectID string, limit int) ([]Overview, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT grouped.conversation_id,
		       COALESCE(
		           NULLIF(metadata.title, ''),
		           NULLIF((
		               SELECT LEFT(BTRIM(message.content), 80)
		               FROM conversation_messages message
		               WHERE message.user_id = $1
		                 AND message.conversation_id = grouped.conversation_id
		                 AND message.role = $4
		               ORDER BY message.id
		               LIMIT 1
		           ), ''),
		           'New chat'
		       ),
		       COALESCE(metadata.pinned, FALSE),
		       metadata.project_id,
		       grouped.updated_at
		FROM (
		    SELECT conversation_id, MAX(created_at) AS updated_at
		    FROM conversation_messages
		    WHERE user_id = $1
		    GROUP BY conversation_id
		) grouped
		JOIN conversation_metadata metadata
		  ON metadata.user_id = $1
		 AND metadata.conversation_id = grouped.conversation_id
		WHERE metadata.project_id = $2
		ORDER BY metadata.pinned DESC, grouped.updated_at DESC
		LIMIT $3
	`, userID, projectID, limit, RoleUser)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Overview{}
	for rows.Next() {
		var item Overview
		if err := rows.Scan(&item.ID, &item.Title, &item.Pinned, &item.ProjectID, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateMetadata(ctx context.Context, userID, conversationID string, update MetadataUpdate) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE user_id = $1 AND conversation_id = $2)`, userID, conversationID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrConversationNotFound
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversation_metadata (user_id, conversation_id, title, pinned, project_id)
		VALUES ($1, $2, COALESCE($3, ''), COALESCE($4, FALSE), COALESCE($5, ''))
		ON CONFLICT (user_id, conversation_id) DO UPDATE SET
			title = CASE WHEN $3::TEXT IS NULL THEN conversation_metadata.title ELSE $3 END,
			pinned = CASE WHEN $4::BOOLEAN IS NULL THEN conversation_metadata.pinned ELSE $4 END,
			project_id = CASE WHEN $5::TEXT IS NULL THEN conversation_metadata.project_id ELSE $5 END
	`, userID, conversationID, update.Title, update.Pinned, update.ProjectID)
	return err
}

func (s *PostgresStore) CreateProject(ctx context.Context, userID string, project Project) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (user_id, project_id, name, description, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, userID, project.ID, project.Name, project.Description, project.CreatedAt)
	return err
}

func (s *PostgresStore) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, name, description, created_at
		FROM projects
		WHERE user_id = $1
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var project Project
		if err := rows.Scan(&project.ID, &project.Name, &project.Description, &project.CreatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func (s *PostgresStore) GetProject(ctx context.Context, userID, projectID string) (Project, error) {
	var project Project
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id, name, description, created_at
		FROM projects
		WHERE user_id = $1 AND project_id = $2
	`, userID, projectID).Scan(&project.ID, &project.Name, &project.Description, &project.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrConversationNotFound
	}
	return project, err
}

func (s *PostgresStore) Delete(ctx context.Context, userID, conversationID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM conversation_messages WHERE user_id = $1 AND conversation_id = $2`, userID, conversationID)
	if err != nil {
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrConversationNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_summaries WHERE user_id = $1 AND conversation_id = $2`, userID, conversationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_metadata WHERE user_id = $1 AND conversation_id = $2`, userID, conversationID); err != nil {
		return err
	}
	return tx.Commit()
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
