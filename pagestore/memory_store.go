package pagestore

import (
	"context"
	"sort"
	"sync"
	"time"
)

// InMemoryStore is the Store for tests and database-less local runs. It
// has no view of which chats exist, so Sweep skips the orphan rule; the
// server deletes a chat's pages explicitly when the chat is deleted.
type InMemoryStore struct {
	mu    sync.Mutex
	pages map[string]Page
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{pages: map[string]Page{}}
}

func memKey(userID, conversationID, url string) string {
	return userID + "\x00" + conversationID + "\x00" + url
}

func (s *InMemoryStore) Get(_ context.Context, userID, conversationID, url string, notBefore time.Time) (Page, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pages[memKey(userID, conversationID, url)]
	if !ok || p.FetchedAt.Before(notBefore) {
		return Page{}, false, nil
	}
	return p, true, nil
}

func (s *InMemoryStore) Put(_ context.Context, page Page) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages[memKey(page.UserID, page.ConversationID, page.URL)] = page
	return nil
}

func (s *InMemoryStore) DeleteConversation(_ context.Context, userID, conversationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, p := range s.pages {
		if p.UserID == userID && p.ConversationID == conversationID {
			delete(s.pages, key)
		}
	}
	return nil
}

func (s *InMemoryStore) Sweep(_ context.Context, expiredBefore, _ time.Time, maxBytes int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted int64
	var kept []string
	for key, p := range s.pages {
		if p.FetchedAt.Before(expiredBefore) {
			delete(s.pages, key)
			deleted++
			continue
		}
		kept = append(kept, key)
	}
	// Newest first; everything past the budget goes.
	sort.Slice(kept, func(i, j int) bool { return s.pages[kept[i]].FetchedAt.After(s.pages[kept[j]].FetchedAt) })
	var total int64
	for _, key := range kept {
		total += s.pages[key].Bytes()
		if total > maxBytes {
			delete(s.pages, key)
			deleted++
		}
	}
	return deleted, nil
}

// Len reports how many pages are stored (for tests).
func (s *InMemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pages)
}
