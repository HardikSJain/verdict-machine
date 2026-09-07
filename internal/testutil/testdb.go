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

// dbTestLockKey is an arbitrary fixed Postgres advisory-lock key shared by
// every Pool(t) call across every package. Every test package's test binary
// connects to the same physical VERDICT_TEST_DATABASE_URL database, and each
// Pool(t) call TRUNCATEs it (with RESTART IDENTITY) as part of setting up an
// empty database for that test. go test runs different packages' test
// binaries concurrently by default, so without coordination one package's
// TRUNCATE ... RESTART IDENTITY can land in the middle of another package's
// symbol_id-assigning insert loop (EnsureSymbols, internal/market/store.go):
// the identity sequence resets mid-loop and two different ISINs in that same
// call can end up mapped to the same symbol_id, aborting the insert with a
// bars_pkey unique-violation. Holding this lock for a Pool's whole lifetime
// (acquire before TRUNCATE, release after the test's pool closes) serializes
// every Pool(t)-backed test, across every package, against every other one.
const dbTestLockKey = 84031177

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

	// Hold dbTestLockKey on a dedicated connection for this pool's whole
	// lifetime; see its doc comment. t.Cleanup runs LIFO, and pool.Close was
	// registered above, before this: unlock+Release below runs first, then
	// pool.Close — the lock is held until this test is fully done with pool,
	// and the acquired connection is freed before the pool that owns it closes.
	lockConn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	_, err = lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", dbTestLockKey)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", dbTestLockKey)
		lockConn.Release()
	})

	// TRUNCATE does not fire row-level triggers, so the insert-only guard does not block it.
	_, err = pool.Exec(ctx, "TRUNCATE bars, symbols, ingest_log RESTART IDENTITY")
	require.NoError(t, err)
	return pool
}
