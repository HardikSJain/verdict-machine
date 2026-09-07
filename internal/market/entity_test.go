package market_test

import (
	"context"
	"fmt"
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

// memberOf returns the ranked member carrying ticker, failing the test when
// the universe does not hold it.
func memberOf(t *testing.T, members []market.UniverseMember, ticker string) market.UniverseMember {
	t.Helper()
	for _, m := range members {
		if m.Ticker == ticker {
			return m
		}
	}
	t.Fatalf("%s not found in universe", ticker)
	return market.UniverseMember{}
}

// insertBarDirect writes one EQ bar under a given symbol_id without going
// through InsertBars, which would register the ISIN and so write the very
// symbols row a sentinel-valid_from fixture exists to avoid.
func insertBarDirect(t *testing.T, pool *pgxpool.Pool, symbolID int64, source string, d time.Time, close float64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO bars (symbol_id, date, source, series, open, high, low, close, volume, turnover, content_hash)
		 VALUES ($1, $2, $3, 'EQ', $4, $4, $4, $4, 1000, $5, $6)`,
		symbolID, d, source, close, close*1000,
		[]byte(fmt.Sprintf("%d|%s|%s|%.2f", symbolID, source, d.Format("2006-01-02"), close)))
	require.NoError(t, err)
}

// TestBarsForDate_ErrorsWhenTwoMembersTradeOnOneDate is design §8.5.
//
// Two members of one entity holding a bar for the same source and session is
// the catastrophic false positive: two genuinely different companies merged
// by a bad roster line, in a store that cannot delete. Silently collapsing to
// one bar would halve a company's truth and look exactly like a normal read,
// so the guard errors instead. It is a per-read check rather than a seed-time
// one so that it keeps checking forever.
//
// The second half is the guard's boundary. eod2 and nse-bhavcopy legitimately
// both carry a bar for one entity on one date -- that is two sources
// describing one session, not two instruments trading -- and BarsForDate
// filters by source, so it must not fire there.
func TestBarsForDate_ErrorsWhenTwoMembersTradeOnOneDate(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()
	const (
		predISIN = "INE081A01012"
		succISIN = "INE081A01020"
	)
	overlap := market.Day(2022, 7, 29)
	clean := market.Day(2022, 8, 1)

	_, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{
		barAt(predISIN, "TATASTEEL", overlap, 959.40),
		barAt(succISIN, "TATASTEEL", overlap, 100.35),
		barAt(succISIN, "TATASTEEL", clean, 102.00),
	})
	require.NoError(t, err)
	_, err = store.InsertBars(ctx, market.SourceEod2, []market.Bar{
		barAt(predISIN, "TATASTEEL", clean, 10.20),
	})
	require.NoError(t, err)

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: predISIN, Ticker: "TATASTEEL", Date: overlap},
		{ISIN: succISIN, Ticker: "TATASTEEL", Date: overlap},
	})
	require.NoError(t, err)
	pred, succ := ids[predISIN], ids[succISIN]

	// Unlinked, the two ISINs are two companies and the shared session is
	// unremarkable. This is also the baseline the guard must not disturb.
	before, err := store.BarsForDate(ctx, market.SourceBhavcopy, overlap, time.Now())
	require.NoError(t, err)
	require.Len(t, before, 2)

	linkRow(t, pool, succ, pred, overlap)

	got, err := store.BarsForDate(ctx, market.SourceBhavcopy, overlap, time.Now())
	require.Error(t, err, "two members of one entity on one session in one source must scream, not collapse")
	require.Nil(t, got, "a read that cannot be trusted returns nothing, not a plausible-looking half of the truth")
	require.ErrorContains(t, err, fmt.Sprint(pred), "the error must name the entity")
	require.ErrorContains(t, err, "2022-07-29", "the error must name the date")
	require.ErrorContains(t, err, market.SourceBhavcopy, "the error must name the source")

	// Same entity, same date, different sources: no error, one bar each.
	eod, err := store.BarsForDate(ctx, market.SourceEod2, clean, time.Now())
	require.NoError(t, err, "cross-source coexistence is expected; BarsForDate filters by source")
	require.Len(t, eod, 1)
	require.Equal(t, pred, eod[0].SymbolID, "the eod2 row is still physically the predecessor's")
	require.Equal(t, pred, eod[0].EntityID)
	require.Equal(t, succISIN, eod[0].ISIN,
		"the label is the member in force on 2022-08-01, which is post-boundary; the row's own symbol does not decide it")

	bhav, err := store.BarsForDate(ctx, market.SourceBhavcopy, clean, time.Now())
	require.NoError(t, err)
	require.Len(t, bhav, 1)
	require.Equal(t, succ, bhav[0].SymbolID)
	require.Equal(t, pred, bhav[0].EntityID)
}

// TestUniverseAsOf_DaysPresentCountsSessionsNotRows_AndErrorsOnOverlap is
// design §8.11.
//
// Grouping by entity forces presence to be counted in SESSIONS: a union of
// two members' spans is what lets a split company clear the 80% gate that
// neither half can clear alone (phase 1). But counting DISTINCT dates and
// stopping there deletes the only overlap detector on the read M1 actually
// calls -- UniverseAsOf is called on every rebalance date, BarsForDate has
// zero non-test callers -- so count(*) is kept beside it as member_rows and
// the difference is an error (phase 2). Without it, a bad link merging two
// concurrently-trading companies returns a median over two companies'
// turnovers, neatly, with no error anywhere.
func TestUniverseAsOf_DaysPresentCountsSessionsNotRows_AndErrorsOnOverlap(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()

	// Phase 1 -- a clean succession. Five sessions each over a ten-session
	// window: 5 < ceil(0.8 * 10) = 8, so neither fragment qualifies alone.
	var sessions []time.Time
	for i := 0; i < 10; i++ {
		sessions = append(sessions, market.Day(2023, 1, 2).AddDate(0, 0, i))
	}
	var bars []market.Bar
	for i, d := range sessions {
		bars = append(bars, barAt("INE0ANCHOR012", "ANCHOR", d, 700))
		if i < 5 {
			bars = append(bars, barAt("INE081A01012", "TATASTEEL", d, 900+float64(i)))
		} else {
			bars = append(bars, barAt("INE081A01020", "TATASTEEL", d, 100+float64(i)))
		}
	}
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, bars)
	require.NoError(t, err)
	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE081A01012", Ticker: "TATASTEEL", Date: sessions[0]},
		{ISIN: "INE081A01020", Ticker: "TATASTEEL", Date: sessions[9]},
	})
	require.NoError(t, err)
	pred, succ := ids["INE081A01012"], ids["INE081A01020"]

	before, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, sessions[9], 10, 500, time.Now())
	require.NoError(t, err)
	require.Equal(t, 10, before.Sessions)
	require.True(t, tickers(before.Members)["ANCHOR"])
	require.False(t, tickers(before.Members)["TATASTEEL"],
		"unlinked, each fragment holds 5 of 10 sessions against a requirement of 8; this is the defect the design exists for")

	linkRow(t, pool, succ, pred, sessions[5])

	after, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, sessions[9], 10, 500, time.Now())
	require.NoError(t, err)
	m := memberOf(t, after.Members, "TATASTEEL")
	require.Equal(t, pred, m.EntityID, "the entity is named by the chain root")
	require.Equal(t, succ, m.SymbolID, "SymbolID is the member in force on asOf, which is post-boundary here")
	require.Equal(t, "INE081A01020", m.ISIN)
	require.Equal(t, 10, m.DaysPresent, "ten sessions counted once each; a row count would say ten too, which is why phase 2 exists")
	require.Equal(t, 2, m.Fragments, "joining SymbolID straight back to bars.symbol_id would miss half the window")
	require.NotNil(t, m.LastBreak, "a consumer computing a return across this window must be told there is a boundary in it")
	require.Equal(t, sessions[5], *m.LastBreak)
	require.Equal(t, 1, memberOf(t, after.Members, "ANCHOR").Fragments)
	require.Nil(t, memberOf(t, after.Members, "ANCHOR").LastBreak)

	// Phase 2 -- the catastrophic case, in a window that does not overlap
	// phase 1's. ACME's two "members" both trade on session 5.
	var win []time.Time
	for i := 0; i < 10; i++ {
		win = append(win, market.Day(2024, 1, 2).AddDate(0, 0, i))
	}
	var bad []market.Bar
	for _, d := range win {
		bad = append(bad, barAt("INE0ACME01012", "ACME", d, 500))
	}
	bad = append(bad, barAt("INE0ACME01020", "ACME", win[4], 50))
	_, err = store.InsertBars(ctx, market.SourceBhavcopy, bad)
	require.NoError(t, err)
	badIDs, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE0ACME01012", Ticker: "ACME", Date: win[0]},
		{ISIN: "INE0ACME01020", Ticker: "ACME", Date: win[4]},
	})
	require.NoError(t, err)
	linkRow(t, pool, badIDs["INE0ACME01020"], badIDs["INE0ACME01012"], win[4])

	_, err = store.UniverseAsOf(ctx, market.SourceBhavcopy, win[9], 10, 500, time.Now())
	require.Error(t, err, "member_rows > days_present is a false-positive link and must not be absorbed into a percentile")
	require.ErrorContains(t, err, fmt.Sprint(badIDs["INE0ACME01012"]), "the error must name the entity")
	require.ErrorContains(t, err, "11 member rows", "count(*) is kept beside count(DISTINCT date) precisely so this number exists")
	require.ErrorContains(t, err, "10 distinct sessions", "the session is counted ONCE; a row count would read 11 and inflate past the 80% gate")
	require.ErrorContains(t, err, "2024-01-11", "the error must name the window")
}

// TestEntityLabelWithSentinelValidFrom is design §8.13 -- the test that would
// have caught revision 1's fatal defect before a line of code was written.
//
// Every symbols row here is written by direct INSERT at
// valid_from = 0001-01-01, and that is required rather than convenient:
// EnsureSymbols rejects a zero date by design (it would collide with
// migration 0002's pre-migration sentinel), so the writer CANNOT produce the
// state 3,745 of 4,100 live symbol_ids are in, and a fixture built through it
// is testing a database that does not exist. The successor is registered
// first so the predecessor's ingested_at is the LATER one -- the exact
// ordering that made TATASTEEL, ICICIBANK, SBIN and AXISBANK resolve to dead
// ISINs under a rule that ordered members by valid_from and broke ties on
// ingested_at DESC.
//
// This is the only Stage 1 test that exercises the LINKED label path with the
// production symbols shape, which is why the staging plan puts it here rather
// than waiting for the roster.
func TestEntityLabelWithSentinelValidFrom(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()

	pre := market.Day(2015, 1, 2)
	post := market.Day(2022, 10, 31)
	boundary := market.Day(2022, 7, 29)

	register := func(isin string) int64 {
		var id int64
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO symbols (isin, ticker, valid_from) VALUES ($1, 'TATASTEEL', DATE '0001-01-01')
			 RETURNING symbol_id`, isin).Scan(&id))
		return id
	}
	succ := register("INE081A01020")
	time.Sleep(5 * time.Millisecond)
	pred := register("INE081A01012")

	insertBarDirect(t, pool, pred, market.SourceBhavcopy, pre, 410.75)
	insertBarDirect(t, pool, succ, market.SourceBhavcopy, post, 105.00)
	// eod2.LoadDir files a ticker's whole CSV under today's ISIN, so the 2015
	// eod2 bar is physically the SUCCESSOR's row.
	insertBarDirect(t, pool, succ, market.SourceEod2, pre, 41.10)

	linkRow(t, pool, succ, pred, boundary)

	preBars, err := store.BarsForDate(ctx, market.SourceBhavcopy, pre, time.Now())
	require.NoError(t, err)
	require.Len(t, preBars, 1)
	require.Equal(t, "INE081A01012", preBars[0].ISIN, "on 2015-01-02 the ISIN really was INE081A01012")
	require.Equal(t, pred, preBars[0].SymbolID)
	require.Equal(t, pred, preBars[0].EntityID)

	postBars, err := store.BarsForDate(ctx, market.SourceBhavcopy, post, time.Now())
	require.NoError(t, err)
	require.Len(t, postBars, 1)
	require.Equal(t, "INE081A01020", postBars[0].ISIN,
		"the label tracks the query date, not which member the backfill happened to register last")
	require.Equal(t, succ, postBars[0].SymbolID)
	require.Equal(t, pred, postBars[0].EntityID)

	// The strongest assertion in the file: a row filed under the successor,
	// on a pre-boundary session, must come back under the predecessor's ISIN.
	// A per-symbol label rule returns INE081A01020 here and cannot do
	// otherwise, because the row it is labelling is the successor's.
	eodBars, err := store.BarsForDate(ctx, market.SourceEod2, pre, time.Now())
	require.NoError(t, err)
	require.Len(t, eodBars, 1)
	require.Equal(t, succ, eodBars[0].SymbolID, "the row is physically the successor's and stays that way")
	require.Equal(t, pred, eodBars[0].EntityID)
	require.Equal(t, "INE081A01012", eodBars[0].ISIN,
		"the member in force on the session decides the label, not the member holding the row")

	// The same rule, through the read M1 actually calls.
	early, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, pre, 1, 10, time.Now())
	require.NoError(t, err)
	require.Len(t, early.Members, 1)
	require.Equal(t, "INE081A01012", early.Members[0].ISIN)
	require.Equal(t, pred, early.Members[0].SymbolID)
	require.Equal(t, pred, early.Members[0].EntityID)
	require.Nil(t, early.Members[0].LastBreak, "a 2022 boundary is in the future of a 2015 query and must not be reported")

	late, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, post, 1, 10, time.Now())
	require.NoError(t, err)
	require.Len(t, late.Members, 1)
	require.Equal(t, "INE081A01020", late.Members[0].ISIN)
	require.Equal(t, succ, late.Members[0].SymbolID)
	require.Equal(t, pred, late.Members[0].EntityID)
	require.NotNil(t, late.Members[0].LastBreak)
	require.Equal(t, boundary, *late.Members[0].LastBreak)
}

// TestEntityBoundariesAndInvariantsOnAWellFormedEntity covers the two store
// methods M1 and the monthly ops item will call, on the only configuration
// Stage 1 can build: one hand-made, well-formed entity.
//
// It is NOT design §8.7, the negative control, and must not be read as
// standing in for it. Nothing here injects a bogus link, so nothing here is
// evidence that the invariant checker can FAIL -- which is the only property
// worth having from a safety net. §8.7 lands with the seeder in Stage 2,
// including §8.7b, the case that documents what the net cannot catch at all.
func TestEntityBoundariesAndInvariantsOnAWellFormedEntity(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()
	boundary := market.Day(2022, 7, 29)

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE081A01012", Ticker: "TATASTEEL", Date: market.Day(2015, 1, 2)},
		{ISIN: "INE081A01020", Ticker: "TATASTEEL", Date: market.Day(2022, 10, 31)},
	})
	require.NoError(t, err)
	pred, succ := ids["INE081A01012"], ids["INE081A01020"]

	before := time.Now()
	time.Sleep(10 * time.Millisecond)
	linkRow(t, pool, succ, pred, boundary)

	early, err := store.EntityBoundaries(ctx, []int64{pred}, before)
	require.NoError(t, err)
	require.Empty(t, early, "pinned before the link's ingest the entity has no boundary, because it has no members")

	got, err := store.EntityBoundaries(ctx, []int64{pred}, time.Now())
	require.NoError(t, err)
	require.Len(t, got[pred], 1)
	require.Equal(t, market.EntityBoundary{
		Date:            boundary,
		PredecessorID:   pred,
		SuccessorID:     succ,
		PredecessorISIN: "INE081A01012",
		SuccessorISIN:   "INE081A01020",
	}, got[pred][0])

	none, err := store.EntityBoundaries(ctx, nil, time.Now())
	require.NoError(t, err)
	require.Empty(t, none)

	violations, err := store.CheckEntityInvariants(ctx, time.Now())
	require.NoError(t, err)
	require.Empty(t, violations, "a flat, disjoint, same-issuer entity violates nothing")
}
