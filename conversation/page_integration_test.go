//go:build integration

package conversation

import (
	"neochat/internal/dbtest"
	"testing"
)

func TestPostgresHistoryPagination(t *testing.T) {
	exercisePages(t, NewPostgresStore(dbtest.Postgres(t)))
}
