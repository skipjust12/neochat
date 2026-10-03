package attachment

import (
	"context"
	"sync"
	"time"
)

// InMemoryStore is the process-local Store used by tests and the dev
// harness; cmd/server uses PostgresStore.
type InMemoryStore struct {
	mu    sync.Mutex
	files map[string]File // key: userID + "\x00" + id
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{files: map[string]File{}}
}

func memKey(userID, id string) string { return userID + "\x00" + id }

func (s *InMemoryStore) Put(_ context.Context, f File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f.Data = append([]byte(nil), f.Data...)
	s.files[memKey(f.UserID, f.ID)] = f
	return nil
}

func (s *InMemoryStore) Get(_ context.Context, userID, id string) (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[memKey(userID, id)]
	if !ok {
		return File{}, ErrNotFound
	}
	return f, nil
}

func (s *InMemoryStore) Claim(_ context.Context, userID, conversationID string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		key := memKey(userID, id)
		if f, ok := s.files[key]; ok && f.ConversationID == "" {
			f.ConversationID = conversationID
			s.files[key] = f
		}
	}
	return nil
}

func (s *InMemoryStore) DeleteConversation(_ context.Context, userID, conversationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, f := range s.files {
		if f.UserID == userID && f.ConversationID == conversationID && conversationID != "" {
			delete(s.files, key)
		}
	}
	return nil
}

func (s *InMemoryStore) PruneUnclaimed(_ context.Context, userID string, cutoff time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, f := range s.files {
		if f.UserID == userID && f.ConversationID == "" && f.CreatedAt.Before(cutoff) {
			delete(s.files, key)
		}
	}
	return nil
}

func (s *InMemoryStore) CountUnclaimed(_ context.Context, userID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.files {
		if f.UserID == userID && f.ConversationID == "" {
			n++
		}
	}
	return n, nil
}
