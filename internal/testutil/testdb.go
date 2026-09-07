// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/db"
)

// Pool returns a migrated, empty test database. It skips under -short and
// fails loudly when VERDICT_TEST_DATABASE_URL is unset, so a missing database
// is never mistaken for a passing run.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping database test")
	}
	url := os.Getenv("VERDICT_TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("VERDICT_TEST_DATABASE_URL is not set; run `make db-up` then `make test`")
	}
	ctx := context.Background()
	require.NoError(t, db.Migrate(ctx, url))
	pool, err := db.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// TRUNCATE does not fire row-level triggers, so the insert-only guard does not block it.
	_, err = pool.Exec(ctx, "TRUNCATE bars, symbols, ingest_log RESTART IDENTITY")
	require.NoError(t, err)
	return pool
}
