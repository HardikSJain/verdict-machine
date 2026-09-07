// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/db"
)

// dbTestLockKey is an arbitrary fixed Postgres advisory-lock key shared by
// every Pool(t) call across every package. Every test package's test binary
// connects to the same physical VERDICT_TEST_DATABASE_URL database, and each
// Pool(t) call migrates it and TRUNCATEs it (with RESTART IDENTITY) as part
// of setting up an empty database for that test. go test runs different
// packages' test binaries concurrently by default, so without coordination
// one package's TRUNCATE ... RESTART IDENTITY can land in the middle of
// another package's symbol_id-assigning insert loop (EnsureSymbols,
// internal/market/store.go): the identity sequence resets mid-loop and two
// different ISINs in that same call can end up mapped to the same symbol_id,
// aborting the insert with a bars_pkey unique-violation.
//
// The lock covers the migration as well as the TRUNCATE, and it has to. On a
// database that is already migrated, db.Migrate is a harmless no-op and the
// gap is invisible; on a fresh one -- which is the very first `make test` on
// a clean clone, because docker/initdb creates verdict_test empty and
// `make migrate` targets the other database -- three test binaries would run
// goose.UpContext concurrently with nothing serializing them. goose's legacy
// Up path takes no lock of its own and its Postgres dialect issues a bare
// CREATE TABLE goose_db_version with no IF NOT EXISTS, so either both
// processes fail to create it or both read version 0, both apply 0001, and
// the loser dies on `relation "symbols" already exists`.
//
// Holding this lock for a Pool's whole lifetime -- acquired on its own
// connection before the migration, released after the test's pool closes --
// serializes every Pool(t)-backed test, across every package, against every
// other one.
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

	// Hold dbTestLockKey on a standalone connection for this pool's whole
	// lifetime; see its doc comment. It is taken before db.Migrate, on a
	// connection that does not come from the pool below, because the pool
	// does not exist yet and the migration is inside what the lock protects.
	lockConn, err := pgx.Connect(ctx, url)
	require.NoError(t, err)
	_, err = lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", dbTestLockKey)
	require.NoError(t, err)
	// t.Cleanup runs LIFO and pool.Close is registered after this, so the
	// pool closes first and the lock is released last: it is held until this
	// test is fully done with the database.
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", dbTestLockKey)
		_ = lockConn.Close(context.Background())
	})

	require.NoError(t, db.Migrate(ctx, url))
	pool, err := db.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// TRUNCATE does not fire row-level triggers, so the insert-only guard does not block it.
	_, err = pool.Exec(ctx, "TRUNCATE bars, symbols, ingest_log RESTART IDENTITY")
	require.NoError(t, err)
	return pool
}
