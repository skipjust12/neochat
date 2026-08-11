// Package costlog persists per-request cost records so real spend can be
// analyzed after the fact -- billing reconciliation, unit-economics
// sanity checks against docs/unit-economics.md, eval-set cost tracking --
// instead of only ever existing as an in-process aggregate. See the
// "per-request cost accounting" item in README.md's pre-launch checklist.
//
// This is deliberately a separate package from router, which only builds
// router.CostLogEntry values (see its doc comment) -- router stays pure
// scoring/pricing logic with no I/O, the same way limits/conversation/
// moderation each own the storage seam for their own domain's data.
package costlog

import (
	"context"

	"neochat/router"
)

// Store is the interface seam between this package and wherever cost
// records actually live. InMemoryStore (below) is a deliberately
// throwaway stand-in, the same role limits.SpendStore/InMemorySpendStore
// and moderation.BlockLog/InMemoryBlockLog play for their own data.
//
// Swap-in procedure once a real database exists: implement this interface
// against Postgres/Redis (one row per Record call is the direct
// translation), point callers at the new implementation instead of
// NewInMemoryStore, and delete InMemoryStore. Nothing in server/ depends
// on which implementation is in use, only on this interface.
type Store interface {
	Record(ctx context.Context, entry router.CostLogEntry) error
}
