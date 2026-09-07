package market_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// tryLink appends one succession link row of exactly the shape
// `verdict entities apply` will write in Stage 2, and returns whatever the
// database says. Stage 1 deliberately ships with symbol_links EMPTY, so every
// test that needs the LINKED path -- which is the only path the change is
// actually about -- has to construct its own rows. That is why these are raw
// INSERTs and not a store method: the writer does not exist yet, and waiting
// for it would leave the read path's multi-member behaviour untested for a
// whole stage. Design §9 puts 8.13 in Stage 1 for precisely this reason.
//
// predecessor is set to entity_id because every fixture here is a two-member
// chain whose root is the predecessor; the row-shape CHECK requires it to be
// non-null and distinct from symbol_id for any non-retraction row.
//
// A zero ingestedAt takes the column default, now(). Passing an explicit
// value is how a test pins one side of the ingested_at axis.
func tryLink(pool *pgxpool.Pool, symbolID, entityID int64, boundary, ingestedAt time.Time) error {
	cols := `(symbol_id, entity_id, reason, boundary, predecessor, evidence, roster_sha, seeded_by)`
	vals := `($1, $2, 'succession', $3, $2, '{"fixture": true}'::jsonb, $4, 'stage1 test fixture')`
	args := []any{symbolID, entityID, boundary, []byte{0xAB, 0xCD}}
	if !ingestedAt.IsZero() {
		cols = `(symbol_id, entity_id, reason, boundary, predecessor, evidence, roster_sha, seeded_by, ingested_at)`
		vals = `($1, $2, 'succession', $3, $2, '{"fixture": true}'::jsonb, $4, 'stage1 test fixture', $5)`
		args = append(args, ingestedAt)
	}
	_, err := pool.Exec(context.Background(), `INSERT INTO symbol_links `+cols+` VALUES `+vals, args...)
	return err
}

// linkRow is tryLink for the cases that must succeed.
func linkRow(t *testing.T, pool *pgxpool.Pool, symbolID, entityID int64, boundary time.Time) {
	t.Helper()
	require.NoError(t, tryLink(pool, symbolID, entityID, boundary, time.Time{}))
}

// TestSymbolLinksRejectsATwoHopMap is design §8.6.
//
// Flatness is not a convention here, it is a write-time trigger, and the
// reason is that breaking it produces no error anywhere: the resolver is one
// hop by construction, so a map that says A -> B -> C silently splits one
// company into two entities and every read afterwards is quietly wrong about
// which bars belong together. There is no later check that would notice.
//
// The second half is the partial re-point. Moving an entity to a new root is
// legal, but moving the ROOT while a member is still pointing at it leaves
// that member stranded in an entity of one -- which is the same silent split
// arriving by a different route. The trigger's rule is "re-point every member
// or none", and the last phase asserts the "every" half really is reachable,
// because a guard that rejected the legitimate move as well would be a
// one-way door (design §0 records that revision 1's did exactly that to the
// retraction path).
func TestSymbolLinksRejectsATwoHopMap(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()
	d := market.Day(2020, 1, 2)
	boundary := market.Day(2021, 3, 1)

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE000R01011", Ticker: "ROOT", Date: d},
		{ISIN: "INE000A01012", Ticker: "AAA", Date: d},
		{ISIN: "INE000B01013", Ticker: "BBB", Date: d},
		{ISIN: "INE000O01014", Ticker: "OTHER", Date: d},
	})
	require.NoError(t, err)
	root, a, b, other := ids["INE000R01011"], ids["INE000A01012"], ids["INE000B01013"], ids["INE000O01014"]

	linkRow(t, pool, a, root, boundary)

	// Two hops: B -> A, where A itself already resolves to ROOT.
	err = tryLink(pool, b, a, boundary, time.Time{})
	require.ErrorContains(t, err, "itself resolves to",
		"a chain A->B->C must be rejected at write time; the resolver never follows a second hop, so this would split one entity in two with no error anywhere")

	// Re-pointing the root while A still hangs off it is a partial re-point.
	err = tryLink(pool, root, other, boundary, time.Time{})
	require.ErrorContains(t, err, "is the entity of other symbols")

	// Same rejection with two members hanging off the root.
	linkRow(t, pool, b, root, boundary)
	err = tryLink(pool, root, other, boundary, time.Time{})
	require.ErrorContains(t, err, "is the entity of other symbols")

	// Re-pointing EVERY member first, then the root, is the legitimate move
	// and must be allowed: the guard is an ordering rule, not a prohibition.
	linkRow(t, pool, a, other, boundary)
	linkRow(t, pool, b, other, boundary)
	require.NoError(t, tryLink(pool, root, other, boundary, time.Time{}),
		"once no member points at the root any more, the root may follow them")

	var members []int64
	rows, err := pool.Query(ctx,
		`SELECT symbol_id FROM (
		     SELECT DISTINCT ON (symbol_id) symbol_id, entity_id FROM symbol_links
		     ORDER BY symbol_id, ingested_at DESC
		 ) m WHERE entity_id = $1 ORDER BY symbol_id`, other)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		members = append(members, id)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, []int64{root, a, b}, members, "the whole entity moved, not part of it")
}

// TestEntityMapAtIsPinnedAndEntityMapNowIsNot is design §8.15.
//
// entity_map_at(ts) is the resolution rule published for a notebook that
// wants entities without reimplementing the Go CTEs, and its whole value is
// the ingested_at bound: a caller who pinned UniverseAsOf at T and then joins
// its EntityID back to bars through a map resolved at now() gets an answer
// that silently changes the next time a split extends an entity or a
// retraction dissolves one. Revision 1 published only the unpinned view and
// pointed consumers at it; §7's replay guarantee does not survive that.
//
// entity_map_now is asserted here too, and asserted to be UNPINNED. It is not
// a bug -- it is the interactive convenience, and the test exists so that the
// difference between the two is a fact in the suite rather than a paragraph.
func TestEntityMapAtIsPinnedAndEntityMapNowIsNot(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE081A01012", Ticker: "TATASTEEL", Date: market.Day(2015, 1, 2)},
		{ISIN: "INE081A01020", Ticker: "TATASTEEL", Date: market.Day(2022, 10, 31)},
	})
	require.NoError(t, err)
	pred, succ := ids["INE081A01012"], ids["INE081A01020"]

	t0 := time.Now()
	time.Sleep(10 * time.Millisecond)
	linkRow(t, pool, succ, pred, market.Day(2022, 7, 29))

	mapAt := func(ts time.Time) int64 {
		var got int64
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT entity_id FROM entity_map_at($1) WHERE symbol_id = $2`, ts, succ).Scan(&got))
		return got
	}
	require.Equal(t, succ, mapAt(t0),
		"pinned before the link was ingested, the successor is still its own entity; a replay of that moment must not see a mapping that did not exist yet")
	require.Equal(t, pred, mapAt(time.Now()), "pinned after, it resolves to the entity")

	var now int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT entity_id FROM entity_map_now WHERE symbol_id = $1`, succ).Scan(&now))
	require.Equal(t, pred, now,
		"entity_map_now carries no pin at all -- that is the replay leak, and the name is the only warning a notebook author gets")

	// The root has no link row and resolves to itself on both axes: the
	// COALESCE degeneracy the whole design leans on.
	var rootAt, rootNow int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT entity_id FROM entity_map_at($1) WHERE symbol_id = $2`, t0, pred).Scan(&rootAt))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT entity_id FROM entity_map_now WHERE symbol_id = $1`, pred).Scan(&rootNow))
	require.Equal(t, pred, rootAt)
	require.Equal(t, pred, rootNow)
}
