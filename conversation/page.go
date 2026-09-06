package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

const MaxPageSize = 50
const maxMessageRunes = 65536

func validPage(before int64, limit int) bool { return before >= 0 && limit > 0 && limit <= MaxPageSize }

func (s *InMemoryStore) HistoryPage(ctx context.Context, userID, conversationID string, before int64, limit int) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if !validPage(before, limit) {
		return Page{}, fmt.Errorf("invalid history page")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	page := Page{Messages: []Message{}}
	stored := s.history[conversationKey{userID, conversationID}]
	for i := len(stored) - 1; i >= 0; i-- {
		msg := stored[i]
		if before > 0 && msg.ID >= before {
			continue
		}
		if len(page.Messages) == limit {
			page.NextCursor = page.Messages[len(page.Messages)-1].ID
			break
		}
		runes := []rune(msg.Content)
		if len(runes) > maxMessageRunes {
			msg.Content = string(runes[:maxMessageRunes])
			msg.Truncated = true
		}
		page.Messages = append(page.Messages, msg)
	}
	slices.Reverse(page.Messages)
	return page, nil
}

func (s *PostgresStore) HistoryPage(ctx context.Context, userID, conversationID string, before int64, limit int) (Page, error) {
	if !validPage(before, limit) {
		return Page{}, fmt.Errorf("invalid history page")
	}
	// Bound both row count and large legacy message values at the database.
	rows, err := s.db.QueryContext(ctx, `
 SELECT id,role,LEFT(content,$5),model_id,is_summary,created_at,LENGTH(content)>$5,versions
 FROM conversation_messages
 WHERE user_id=$1 AND conversation_id=$2 AND ($3::bigint=0 OR id<$3)
 ORDER BY id DESC LIMIT $4
 `, userID, conversationID, before, limit+1, maxMessageRunes)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Messages: []Message{}}
	for rows.Next() {
		if len(page.Messages) == limit {
			page.NextCursor = page.Messages[len(page.Messages)-1].ID
			break
		}
		var msg Message
		var encoded []byte
		if err := rows.Scan(&msg.ID, &msg.Role, &msg.Content, &msg.ModelID, &msg.IsSummary, &msg.CreatedAt, &msg.Truncated, &encoded); err != nil {
			return Page{}, err
		}
		if len(encoded) > 0 {
			if err := json.Unmarshal(encoded, &msg.Versions); err != nil {
				return Page{}, err
			}
		}
		page.Messages = append(page.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	slices.Reverse(page.Messages)
	return page, nil
}
