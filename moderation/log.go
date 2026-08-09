package moderation

import (
	"context"
	"sync"
	"time"
)

// BlockEntry is one moderation-flagged request, meant to be persisted so
// block rates and categories can be analyzed over time instead of only
// ever appearing in a single process's stdout log line -- see the
// moderation events item in README's "Next steps".
type BlockEntry struct {
	UserID     string
	Categories []string
	Reason     string
	Timestamp  time.Time
}

// BlockLog records moderation-flagged requests. It is the only seam
// between this package and wherever block events actually live --
// InMemoryBlockLog (below) is a deliberately throwaway stand-in, the same
// role limits.SpendStore/InMemorySpendStore play for spend accounting.
//
// Swap-in procedure once a real server/database exists: implement this
// interface against Postgres/Redis, point callers at the new
// implementation instead of NewInMemoryBlockLog, and delete
// InMemoryBlockLog. Nothing in server/ depends on which implementation is
// in use, only on this interface.
type BlockLog interface {
	Record(ctx context.Context, entry BlockEntry) error
}

// InMemoryBlockLog is a process-local BlockLog backed by a plain slice,
// safe for concurrent use. Nothing persists across a restart and there is
// no cross-instance sharing -- exactly as much "database" as makes sense
// before there is a server to give one a job to do.
type InMemoryBlockLog struct {
	mu      sync.Mutex
	entries []BlockEntry
}

// NewInMemoryBlockLog returns an empty, ready-to-use log.
func NewInMemoryBlockLog() *InMemoryBlockLog {
	return &InMemoryBlockLog{}
}

func (l *InMemoryBlockLog) Record(_ context.Context, entry BlockEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	return nil
}

// Entries returns a copy of every entry recorded so far, oldest first.
// Not part of the BlockLog interface -- a real store would expose this
// through a query, not an in-memory snapshot -- but it's useful for tests
// and for inspecting what's accumulated during local development.
func (l *InMemoryBlockLog) Entries() []BlockEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]BlockEntry, len(l.entries))
	copy(out, l.entries)
	return out
}
