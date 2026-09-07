package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/db"
)

func testURL(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping database test")
	}
	url := os.Getenv("VERDICT_TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("VERDICT_TEST_DATABASE_URL is not set; run `make db-up` then `make test`")
	}
	return url
}

func TestMigrateCreatesInsertOnlyMarketTables(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)
	require.NoError(t, db.Migrate(ctx, url))
	require.NoError(t, db.Migrate(ctx, url), "migrate must be idempotent")

	pool, err := db.Connect(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	for _, table := range []string{"symbols", "bars", "ingest_log"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)", table).Scan(&exists))
		require.True(t, exists, "table %s", table)
	}

	_, err = pool.Exec(ctx, "TRUNCATE bars, symbols RESTART IDENTITY")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO symbols (isin, ticker) VALUES ('INE081A01020', 'TATASTEEL')")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE symbols SET ticker = 'X' WHERE isin = 'INE081A01020'")
	require.ErrorContains(t, err, "insert-only")
	_, err = pool.Exec(ctx, "DELETE FROM symbols WHERE isin = 'INE081A01020'")
	require.ErrorContains(t, err, "insert-only")
}
