package conversation

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

var ErrMessageNotFound = errors.New("conversation: message not found")
var ErrRegenerationLimit = errors.New("conversation: regeneration limit reached")
var ErrConversationNotFound = errors.New("conversation: not found")

// Store is the seam between this package and wherever conversation
// history actually lives. InMemoryStore (below) is a deliberately
// throwaway stand-in -- the same role limits.SpendStore/InMemorySpendStore
// and moderation.BlockLog/InMemoryBlockLog play for their own data.
//
// Swap-in procedure once a real database exists: implement this
// interface against Postgres (a message-per-row table keyed on
// (user_id, conversation_id), ordered by an inserted-at column, is the
// direct translation of InMemoryStore's scheme), point callers at the
// new implementation instead of NewInMemoryStore, and delete
// InMemoryStore. Nothing in server/ depends on which implementation is
// in use, only on this interface.
type Store interface {
	HistoryPage(ctx context.Context, userID, conversationID string, before int64, limit int) (Page, error)
	// Append adds one message to (userID, conversationID)'s history, in
	// order.
	Append(ctx context.Context, userID, conversationID string, msg Message) error
	AddResponseVersion(ctx context.Context, userID, conversationID string, messageID int64, version ResponseVersion) (Message, error)

	// History returns messages recorded for (userID, conversationID),
	// oldest first, skipping the first offset of them. An unknown
	// conversation returns an empty slice, not an error -- a brand new
	// conversation simply has no history yet; so does an offset past the
	// end.
	//
	// offset exists so a caller that already knows the oldest messages are
	// folded into a Summary can avoid reading them back at all: passing
	// Summary.CoversThrough returns exactly the not-yet-summarized
	// remainder (see server.prepare). Without it, every turn of a long
	// conversation read the entire history into memory just to discard
	// most of it -- unbounded work per request, growing with conversation
	// length. Pass 0 for "everything".
	//
	// Because offset is an index into the same oldest-first ordering
	// Summary.CoversThrough counts in, the two stay directly comparable:
	// index i of a result fetched at offset N is absolute index N+i.
	History(ctx context.Context, userID, conversationID string, offset int) ([]Message, error)

	// List returns the user's most recently active conversations. Titles
	// are derived from the first user message in each conversation.
	List(ctx context.Context, userID string, limit int) ([]Overview, error)
	UpdateMetadata(ctx context.Context, userID, conversationID string, update MetadataUpdate) error
	Delete(ctx context.Context, userID, conversationID string) error

	// GetSummary returns the current rolling Summary for (userID,
	// conversationID). An unknown conversation, or one that has never
	// been summarized, returns the zero Summary (CoversThrough 0, Text
	// ""), not an error.
	GetSummary(ctx context.Context, userID, conversationID string) (Summary, error)

	// SetSummary replaces the stored Summary for (userID, conversationID).
	SetSummary(ctx context.Context, userID, conversationID string, s Summary) error
}

// InMemoryStore is a process-local Store backed by a plain map, safe for
// concurrent use. Nothing persists across a restart and there is no
// cross-instance sharing -- exactly as much "database" as makes sense
// before there is a server to give one a job to do.
//
// Keys are (userID, conversationID) pairs, not conversationID alone, so
// one user can never read or extend another user's history even if
// conversation IDs were ever guessed or collided -- the same scoping
// limits.SpendStore already applies to spend, applied here to history.
type InMemoryStore struct {
	mu        sync.Mutex
	history   map[conversationKey][]Message
	summaries map[conversationKey]Summary
	metadata  map[conversationKey]conversationMetadata
}

type conversationMetadata struct {
	title  string
	pinned bool
}

type conversationKey struct {
	userID         string
	conversationID string
}

// NewInMemoryStore returns an empty, ready-to-use store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		history:   make(map[conversationKey][]Message),
		summaries: make(map[conversationKey]Summary),
		metadata:  make(map[conversationKey]conversationMetadata),
	}
}

func (s *InMemoryStore) Append(_ context.Context, userID, conversationID string, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID: userID, conversationID: conversationID}
	msg.ID = int64(len(s.history[key]) + 1)
	if msg.Role == RoleAssistant && len(msg.Versions) == 0 {
		msg.Versions = []ResponseVersion{{Content: msg.Content, ModelID: msg.ModelID, CreatedAt: msg.CreatedAt}}
	}
	s.history[key] = append(s.history[key], msg)
	return nil
}

func (s *InMemoryStore) AddResponseVersion(_ context.Context, userID, conversationID string, messageID int64, version ResponseVersion) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID, conversationID}
	for index := range s.history[key] {
		message := &s.history[key][index]
		if message.ID != messageID || message.Role != RoleAssistant {
			continue
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
		return *message, nil
	}
	return Message{}, ErrMessageNotFound
}

func (s *InMemoryStore) History(_ context.Context, userID, conversationID string, offset int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID: userID, conversationID: conversationID}
	stored := s.history[key]
	// Clamp rather than slice out of range: an offset past the end means
	// "nothing left after what's already summarized", which is an ordinary
	// state (see Store.History's doc comment), not a caller error.
	if offset < 0 {
		offset = 0
	}
	if offset > len(stored) {
		offset = len(stored)
	}
	stored = stored[offset:]
	out := make([]Message, len(stored))
	copy(out, stored)
	return out, nil
}

func (s *InMemoryStore) List(_ context.Context, userID string, limit int) ([]Overview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		return []Overview{}, nil
	}

	out := []Overview{}
	for key, messages := range s.history {
		if key.userID != userID || len(messages) == 0 {
			continue
		}
		meta := s.metadata[key]
		overview := Overview{ID: key.conversationID, Title: meta.title, Pinned: meta.pinned}
		if overview.Title == "" {
			overview.Title = "New chat"
		}
		for _, message := range messages {
			if message.CreatedAt.After(overview.UpdatedAt) {
				overview.UpdatedAt = message.CreatedAt
			}
			if meta.title == "" && overview.Title == "New chat" && message.Role == RoleUser && strings.TrimSpace(message.Content) != "" {
				title := []rune(strings.TrimSpace(message.Content))
				if len(title) > 80 {
					title = title[:80]
				}
				overview.Title = string(title)
			}
		}
		out = append(out, overview)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *InMemoryStore) UpdateMetadata(_ context.Context, userID, conversationID string, update MetadataUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID: userID, conversationID: conversationID}
	if len(s.history[key]) == 0 {
		return ErrConversationNotFound
	}
	meta := s.metadata[key]
	if update.Title != nil {
		meta.title = *update.Title
	}
	if update.Pinned != nil {
		meta.pinned = *update.Pinned
	}
	s.metadata[key] = meta
	return nil
}

func (s *InMemoryStore) Delete(_ context.Context, userID, conversationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID: userID, conversationID: conversationID}
	if len(s.history[key]) == 0 {
		return ErrConversationNotFound
	}
	delete(s.history, key)
	delete(s.summaries, key)
	delete(s.metadata, key)
	return nil
}

func (s *InMemoryStore) GetSummary(_ context.Context, userID, conversationID string) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID: userID, conversationID: conversationID}
	return s.summaries[key], nil
}

func (s *InMemoryStore) SetSummary(_ context.Context, userID, conversationID string, summary Summary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey{userID: userID, conversationID: conversationID}
	s.summaries[key] = summary
	return nil
}
