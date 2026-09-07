package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/db"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
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

	// testutil.Pool migrates (again; idempotent, see above), connects, and
	// truncates under the shared cross-package advisory lock — the same path
	// every other package's database test uses, so this test's own TRUNCATE
	// can never interleave with another package's symbol_id-assigning insert
	// loop. See testutil.Pool's doc comment.
	pool := testutil.Pool(t)

	for _, table := range []string{"symbols", "bars", "ingest_log"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)", table).Scan(&exists))
		require.True(t, exists, "table %s", table)
	}

	_, err := pool.Exec(ctx,
		"INSERT INTO symbols (isin, ticker, valid_from) VALUES ('INE081A01020', 'TATASTEEL', '2015-06-30')")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE symbols SET ticker = 'X' WHERE isin = 'INE081A01020'")
	require.ErrorContains(t, err, "insert-only")
	_, err = pool.Exec(ctx, "DELETE FROM symbols WHERE isin = 'INE081A01020'")
	require.ErrorContains(t, err, "insert-only")

	// bars carries its own insert-only trigger, separate from symbols'; exercise it
	// directly rather than relying on the symbols assertions above to stand in for it.
	// symbol_id 1 is deterministic here: RESTART IDENTITY above reset the sequence and
	// exactly one symbols row (inserted just above) has been written since.
	_, err = pool.Exec(ctx,
		`INSERT INTO bars (symbol_id, date, source, series, open, high, low, close, volume, content_hash)
		 VALUES (1, '2024-01-01', 'nse-bhavcopy', 'EQ', 100, 105, 95, 102, 1000, $1)`,
		[]byte{0})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE bars SET close = 999 WHERE symbol_id = 1")
	require.ErrorContains(t, err, "insert-only")
	_, err = pool.Exec(ctx, "DELETE FROM bars WHERE symbol_id = 1")
	require.ErrorContains(t, err, "insert-only")
}
