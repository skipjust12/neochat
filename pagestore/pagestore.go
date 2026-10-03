// Package pagestore keeps the pages the model read with web_fetch, per
// chat, so a later message can read further into a long page or come back
// to one without fetching it again.
//
// Nothing here may pile up: a chat's pages are deleted with the chat
// (conversation.PostgresStore.Delete does it in the same transaction), and
// Sweep -- run hourly by the server -- drops pages older than TTL, pages of
// chats that were never stored or no longer exist, and then the oldest
// pages until the rest fit a total size budget. Incognito chats never
// store pages at all (the server's choice, not this package's).
package pagestore

import (
	"context"
	"time"
)

// Page is one fetched page of one chat.
type Page struct {
	UserID         string
	ConversationID string
	URL            string // normalized, as requested
	FinalURL       string // after redirects
	Title          string
	Text           string
	Truncated      bool
	FetchedAt      time.Time
}

// Bytes is the size a page counts against the store's budget.
func (p Page) Bytes() int64 { return int64(len(p.Text) + len(p.Title) + len(p.FinalURL) + len(p.URL)) }

const (
	// TTL is how long a page is kept after it was fetched. Pages go stale,
	// and follow-up questions about an answer come within hours.
	TTL = 24 * time.Hour
	// OrphanGrace is how long a page may exist without its chat having any
	// stored message: pages are written while an answer is generated, and
	// a new chat's first messages only after it finishes.
	OrphanGrace = time.Hour
	// DefaultMaxBytes is the default total size budget for all pages.
	DefaultMaxBytes = 256 << 20
)

// Store keeps pages. Implementations are safe for concurrent use.
type Store interface {
	// Get returns the chat's copy of url if it was fetched at or after
	// notBefore.
	Get(ctx context.Context, userID, conversationID, url string, notBefore time.Time) (Page, bool, error)
	// Put stores a page, replacing an earlier copy of the same URL.
	Put(ctx context.Context, page Page) error
	// DeleteConversation drops all pages of a chat.
	DeleteConversation(ctx context.Context, userID, conversationID string) error
	// Sweep deletes pages fetched before expiredBefore, pages of chats that
	// have no stored messages and were fetched before orphanedBefore, and
	// then the least recently fetched pages until the rest take at most
	// maxBytes. It returns how many pages it deleted.
	Sweep(ctx context.Context, expiredBefore, orphanedBefore time.Time, maxBytes int64) (int64, error)
}
