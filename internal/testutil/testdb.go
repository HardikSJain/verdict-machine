// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"os"
	"strings"
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
	lockConn := acquireDBLock(ctx, t, url)
	// t.Cleanup runs LIFO and pool.Close is registered after this, so the
	// pool closes first and the lock is released last: it is held until this
	// test is fully done with the database.
	t.Cleanup(func() { releaseDBLock(lockConn) })

	require.NoError(t, db.Migrate(ctx, url))
	pool, err := db.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// TRUNCATE does not fire row-level triggers, so the insert-only guard does
	// not block it.
	//
	// The table list is read from the catalogue rather than written here, and
	// that is a fix rather than a flourish: it used to be the literal
	// "bars, symbols, symbol_links, ingest_log", and when migration 0005 added
	// index_levels the list silently stopped covering the database. Tests in one
	// package then leaked rows into each other -- a re-ingest that should have
	// been a no-op saw a hash another test had written and skipped, and the
	// failure surfaced three tests away from its cause. Every future table gets
	// covered by construction.
	require.NoError(t, truncateAll(ctx, pool))
	return pool
}

// truncateAll empties every table in the public schema except goose's own
// version table, which must survive or the next Pool would re-run every
// migration.
func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'goose_db_version'
		ORDER BY tablename`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, pgx.Identifier{n}.Sanitize())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	_, err = pool.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" RESTART IDENTITY CASCADE")
	return err
}

// acquireDBLock opens a standalone connection and takes dbTestLockKey on it.
// The connection is deliberately not from any pool: the lock is session-level
// and has to outlive every statement it protects, including the migration
// that runs before a pool exists.
func acquireDBLock(ctx context.Context, t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", dbTestLockKey)
	require.NoError(t, err)
	return conn
}

// releaseDBLock unlocks and closes a connection from acquireDBLock. It uses a
// fresh context because it runs from t.Cleanup, after the test's own context
// may already be done.
func releaseDBLock(conn *pgx.Conn) {
	_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", dbTestLockKey)
	_ = conn.Close(context.Background())
}

// WithDBLock runs fn while holding dbTestLockKey -- the same cross-package
// advisory lock Pool holds -- and releases it before returning.
//
// It exists for the one thing a test may need to do to the shared database
// *outside* a Pool: run db.Migrate directly. An unguarded db.Migrate is safe
// only on an already-migrated database, where it is a no-op. On a virgin one
// -- a fresh clone, which is exactly the newcomer's first `make test` -- it
// races every other package's first Pool call on goose's unguarded
// CREATE TABLE goose_db_version, because go test runs package binaries
// concurrently. See dbTestLockKey.
//
// url is passed in rather than read from the environment here so the caller
// keeps its own skip/fail behaviour for an unset VERDICT_TEST_DATABASE_URL.
func WithDBLock(t *testing.T, url string, fn func()) {
	t.Helper()
	conn := acquireDBLock(context.Background(), t, url)
	defer releaseDBLock(conn)
	fn()
}
