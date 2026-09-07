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
// The second half is the partial re-point, and what this test proves about it
// is narrower than the trigger's own error message says. Moving an entity to a
// new root is legal, but moving the ROOT while a member is still pointing at
// it leaves that member stranded in an entity of one -- the same silent split
// arriving by a different route -- and the trigger rejects it with the
// sentence "re-point every member or none".
//
// Every rejection asserted below writes the ROOT's own row. That is the only
// direction the trigger enforces, and so the only direction this test is
// evidence for: check 1 asks whether the row's new entity_id is itself
// mapped elsewhere, and check 2 asks whether the row's own symbol_id is the
// entity of others. Neither question is ever about the siblings of the symbol
// being written, so the literal case §8.6's clause names -- moving ONE member
// of a multi-member entity to a different root and leaving its siblings
// behind -- is accepted. This test does NOT show otherwise, and
// TestSymbolLinksAcceptsMovingOneMemberOutOfAMultiMemberEntity pins that
// accepted case as the hole it is.
//
// The last phase asserts the "every" half really is reachable, because a
// guard that rejected the legitimate move as well would be a one-way door
// (design §0 records that revision 1's did exactly that to the retraction
// path).
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

	require.ElementsMatch(t, []int64{root, a, b}, linkedMembers(t, pool, other),
		"the whole entity moved, not part of it")
}

// linkedMembers returns the symbol_ids the currently-resolved map points at
// entityID -- the same DISTINCT ON the read path and the flatness trigger
// both use. A root carries no link row of its own, so it appears here only
// once it has been re-pointed at something.
func linkedMembers(t *testing.T, pool *pgxpool.Pool, entityID int64) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT symbol_id FROM (
		     SELECT DISTINCT ON (symbol_id) symbol_id, entity_id FROM symbol_links
		     ORDER BY symbol_id, ingested_at DESC
		 ) m WHERE entity_id = $1 ORDER BY symbol_id`, entityID)
	require.NoError(t, err)
	defer rows.Close()
	var members []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		members = append(members, id)
	}
	require.NoError(t, rows.Err())
	return members
}

// TestSymbolLinksAcceptsMovingOneMemberOutOfAMultiMemberEntity documents a
// hole, in the shape of design §8.7b: it is a test that asserts nothing
// fires, and it exists so the hole stays visible after the paragraph
// describing it is forgotten.
//
// §8.6's second clause is "re-point every member or none", and the trigger
// raises exactly that sentence -- in the ROOT direction only. The literal case
// the clause names is the member direction: take an entity with two members
// hanging off one root and move ONE of them to a different root. The trigger
// accepts it. Check 1 asks whether the new root resolves elsewhere, and it
// does not; check 2 asks whether the symbol being moved is itself the entity
// of other symbols, and it is not. Neither question is about the sibling left
// behind, and there is no third question.
//
// What that produces is the outcome §8.6 exists to prevent, arriving from the
// other side: one company's bars are now filed under two entity ids, every
// read afterwards is quietly right about each half and wrong about the whole,
// and nothing errors -- not the trigger, not I1 (the resulting map is FLAT;
// splitting is not a flatness violation), not I2 (the halves are disjoint by
// construction, which is what made them one entity), not the read path.
//
// This is deliberately not a fix. The design is the authority on the trigger's
// SQL and Stage 1 implemented that SQL correctly; what was wrong was the claim
// made about it. If the guard is ever widened to cover the member direction,
// this test fails and says exactly what changed.
func TestSymbolLinksAcceptsMovingOneMemberOutOfAMultiMemberEntity(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()
	d := market.Day(2020, 1, 2)
	boundary := market.Day(2021, 3, 1)

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE000R01011", Ticker: "ROOT", Date: d},
		{ISIN: "INE000A01012", Ticker: "AAA", Date: d},
		{ISIN: "INE000B01013", Ticker: "BBB", Date: d},
		{ISIN: "INE000N01015", Ticker: "NEWROOT", Date: d},
	})
	require.NoError(t, err)
	root, a, b, newroot := ids["INE000R01011"], ids["INE000A01012"], ids["INE000B01013"], ids["INE000N01015"]

	// One entity, three symbols: root, and two members hanging off it.
	linkRow(t, pool, a, root, boundary)
	linkRow(t, pool, b, root, boundary)
	require.ElementsMatch(t, []int64{a, b}, linkedMembers(t, pool, root),
		"the fixture is a multi-member entity, which is the only shape the clause is about")

	// The counterexample: move A alone. This is what "re-point every member or
	// none" forbids in words, and it is accepted.
	require.NoError(t, tryLink(pool, a, newroot, boundary, time.Time{}),
		"the trigger only ever asks about the row being written and the root it names; moving one member out of a multi-member entity is asked about by neither check")

	require.Equal(t, []int64{a}, linkedMembers(t, pool, newroot),
		"A now belongs to a different entity")
	require.Equal(t, []int64{b}, linkedMembers(t, pool, root),
		"and B is left behind: one company, two entity ids, no error anywhere")

	// The split map is FLAT, which is why no later check catches it either.
	// A -> newroot and B -> root are both one hop; I1 asks about transitivity
	// and has nothing to report, and I2 asks about date overlap between
	// members, which a split can only ever reduce. (I3 does fire, on both
	// halves, but only because these fixture ISINs are four unrelated issuer
	// codes -- it is advisory and it would be silent on the real case, a
	// genuine multi-ISIN company whose members share a prefix.)
	violations, err := store.CheckEntityInvariants(ctx, time.Now())
	require.NoError(t, err)
	var defects []market.Violation
	for _, v := range violations {
		if !v.Advisory() {
			defects = append(defects, v)
		}
	}
	require.Empty(t, defects,
		"neither invariant is a second line of defence here: a split entity is flat, and disjoint, which is all I1 and I2 measure")
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

	// Phase 3 -- the same overlap, ranked OUTSIDE the caller's top-n.
	//
	// The alarm is worth nothing if it only inspects the rows a caller asked
	// to see. BIGCO out-turns ACME ten to one, so at n = 1 the LIMIT keeps
	// BIGCO and drops ACME -- and ACME is the merged entity. Evaluated on the
	// returned rows the read comes back clean: one member, member_rows equal
	// to days_present, no error, no signal anywhere that a link in this store
	// merges two companies. On the live shape that is the normal case, not
	// the corner one: a top-500 call ranks about 2,100 entities and would
	// inspect under a quarter of them.
	var big []market.Bar
	for _, d := range win {
		big = append(big, barAt("INE0BIGCO1012", "BIGCO", d, 5000))
	}
	_, err = store.InsertBars(ctx, market.SourceBhavcopy, big)
	require.NoError(t, err)

	top1, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, win[9], 10, 1, time.Now())
	require.Error(t, err,
		"the overlap alarm must be evaluated over the whole ranking, before LIMIT n; an entity the caller's own n cut off is exactly the one nobody would ever notice")
	require.Empty(t, top1.Members, "a universe that cannot be trusted returns nothing, not its first page")
	require.ErrorContains(t, err, fmt.Sprint(badIDs["INE0ACME01012"]),
		"the error must name the merged entity even though it is not among the returned rows")
	require.ErrorContains(t, err, "11 member rows")
	require.ErrorContains(t, err, "10 distinct sessions")

	// And the same call still succeeds once nothing overlaps: the alarm is
	// pre-LIMIT, not unconditional.
	cleanTop, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, sessions[9], 10, 1, time.Now())
	require.NoError(t, err)
	require.Len(t, cleanTop.Members, 1, "n is still a limit; only the alarm ignores it")
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

	// t0 pins the ingested_at axis on the far side of the link: every bar and
	// every symbols row above it is already in, and the link row below it is
	// not. The reads repeated at t0 at the end of this test must therefore
	// answer exactly what they answered before the link was written. See the
	// block down there for why that matters more than it looks.
	t0 := time.Now()
	time.Sleep(10 * time.Millisecond)
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

	// --- The ingested_at pin, on the read path itself. ---
	//
	// Every assertion above is pinned at time.Now() and so says nothing about
	// the `ingested_at <= <pin>` bound each of the read path's three
	// symbol_links laterals carries -- entityMapCTE's, entityMemberCTE's
	// boundary lateral, and UniverseAsOf's entity_break. Deleting all three
	// left this suite green, which is to say the project's defining promise
	// -- a query pinned before a link was written must not see that link --
	// was unfalsifiable. Each block below names the one bound it stands on
	// and dies if that bound alone is removed.

	// entityMapCTE. With its bound gone the map at t0 already resolves
	// succ -> pred, so this eod2 row -- physically the successor's, on a
	// pre-boundary session -- comes back under an entity that did not exist
	// at the pin.
	eodAtT0, err := store.BarsForDate(ctx, market.SourceEod2, pre, t0)
	require.NoError(t, err)
	require.Len(t, eodAtT0, 1)
	require.Equal(t, succ, eodAtT0[0].SymbolID, "the row is physically the successor's at every pin")
	require.Equal(t, succ, eodAtT0[0].EntityID,
		"pinned before the link was ingested the successor is still its own entity; entity_map's ingested_at bound is the only thing that says so")
	require.Equal(t, "INE081A01020", eodAtT0[0].ISIN,
		"and it is therefore labelled as itself -- the pre-link answer, replayed after the link exists")

	// The same bound, plus UniverseAsOf's entity_break lateral, through the
	// read M1 actually calls. Compare with `late` above: identical arguments
	// but for the pin, and every answer differs.
	lateAtT0, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, post, 1, 10, t0)
	require.NoError(t, err)
	require.Len(t, lateAtT0.Members, 1)
	require.Equal(t, succ, lateAtT0.Members[0].EntityID,
		"entity_map again: at t0 the successor is its own entity, so the universe names it and not the chain root")
	require.Nil(t, lateAtT0.Members[0].LastBreak,
		"entity_break's ingested_at bound: a boundary carried by a link row that did not exist at the pin cannot be reported at that pin")

	// entityMemberCTE's boundary lateral is the third bound, and it takes a
	// symbol carrying MORE THAN ONE link row to observe on its own: with the
	// map's own bound in place every entity at a pre-link pin is a singleton,
	// and a singleton's member is settled before any boundary is read. The
	// resolution rule is ORDER BY ingested_at DESC LIMIT 1 for exactly this
	// reason -- a later row supersedes an earlier one -- so the smallest
	// honest fixture is a corrected boundary: the first row said 2022-07-29,
	// a second row moves it to 2023-04-03.
	//
	// t1 sits between the two, and 2022-10-31 sits between the two boundaries.
	// Resolved at t1 the successor's boundary has passed, so the successor is
	// the member in force and the label is INE081A01020. Resolved at now()
	// instead -- which is what dropping the bound does -- the corrected
	// boundary is still in the future of that session, the successor drops out
	// of the running, and the predecessor's label comes back for a query the
	// correction postdates.
	corrected := market.Day(2023, 4, 3)
	t1 := time.Now()
	time.Sleep(10 * time.Millisecond)
	linkRow(t, pool, succ, pred, corrected)

	atT1, err := store.BarsForDate(ctx, market.SourceBhavcopy, post, t1)
	require.NoError(t, err)
	require.Len(t, atT1, 1)
	require.Equal(t, pred, atT1[0].EntityID, "the link itself is in force at t1; only the boundary moved")
	require.Equal(t, "INE081A01020", atT1[0].ISIN,
		"entity_member's ingested_at bound: the member in force on 2022-10-31 is decided by the boundary the store knew at t1, not by the correction written after it")

	atNow, err := store.BarsForDate(ctx, market.SourceBhavcopy, post, time.Now())
	require.NoError(t, err)
	require.Len(t, atNow, 1)
	require.Equal(t, pred, atNow[0].EntityID)
	require.Equal(t, "INE081A01012", atNow[0].ISIN,
		"and at now() the corrected boundary is in that session's future, so the predecessor is the member -- the two pins genuinely disagree, which is what makes the assertion above load-bearing")
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

// TestEntityInvariantsCatchABogusLink_Overlapping is design 8.7a: the half of
// the negative control that fires.
//
// A proof suite that cannot fail is not a proof suite, so a deliberately
// wrong link -- two known-different companies, merged -- is injected and all
// three detectors are asserted to catch it: the invariant checker, the
// BarsForDate guard and, on the read M1 actually calls, the universe's
// member_rows check.
//
// Read it beside 8.7b, which is the other half and asserts the opposite.
// This half only fires because the two companies TRADE ON THE SAME SESSIONS,
// and the seeder's G3 forbids exactly that at seed time, so what is caught
// here is a hand-written line or a restored dump -- never a link the gates
// admitted.
func TestEntityInvariantsCatchABogusLink_Overlapping(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()

	var sessions []time.Time
	for i := 0; i < 10; i++ {
		sessions = append(sessions, market.Day(2023, 3, 1).AddDate(0, 0, i))
	}
	var bars []market.Bar
	for _, d := range sessions {
		bars = append(bars, barAt("INE888B01018", "ALPHA", d, 400))
		bars = append(bars, barAt("INE777C01011", "BETA", d, 60))
	}
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, bars)
	require.NoError(t, err)
	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE888B01018", Ticker: "ALPHA", Date: sessions[0]},
		{ISIN: "INE777C01011", Ticker: "BETA", Date: sessions[0]},
	})
	require.NoError(t, err)
	alpha, beta := ids["INE888B01018"], ids["INE777C01011"]

	clean, err := store.CheckEntityInvariants(ctx, time.Now())
	require.NoError(t, err)
	require.Empty(t, clean, "two unlinked companies violate nothing")

	linkRow(t, pool, beta, alpha, sessions[3])

	violations, err := store.CheckEntityInvariants(ctx, time.Now())
	require.NoError(t, err)
	require.NotEmpty(t, violations)
	var overlaps, issuers int
	for _, v := range violations {
		switch v.Kind {
		case "overlap":
			overlaps++
			require.Equal(t, alpha, v.EntityID)
			require.False(t, v.Advisory(), "an overlap is a defect, not a question")
		case "issuer":
			issuers++
			require.True(t, v.Advisory())
		default:
			t.Fatalf("unexpected violation %q", v.Kind)
		}
	}
	require.Equal(t, 10, overlaps, "every session the two companies shared is reported")
	require.Equal(t, 1, issuers, "and the issuer codes disagree, which is a question for a human")

	_, err = store.BarsForDate(ctx, market.SourceBhavcopy, sessions[5], time.Now())
	require.ErrorContains(t, err, "two members of one entity cannot trade the same session")

	_, err = store.UniverseAsOf(ctx, market.SourceBhavcopy, sessions[9], 10, 500, time.Now())
	require.Error(t, err, "the universe must refuse a ranking that contains a merged entity")
	require.ErrorContains(t, err, "20 member rows")
	require.ErrorContains(t, err, "10 distinct sessions",
		"without this check the entity comes back with a median over two companies' turnovers and no error anywhere")
}

// TestEntityInvariantsCatchABogusLink_Disjoint is design 8.7b, and it asserts
// that NOTHING fires. It is deliberately a test that documents a hole.
//
// The failure mode design 1 calls the single worst outcome available here --
// a freed ticker picked up by a different company, a reverse merger into a
// listed shell -- is DISJOINT by definition: the old company is dead, so it
// can never produce a colliding bar. Every structural detector in the store
// is keyed on facts the gates already enforce:
//
//   - I2 and the BarsForDate guard need two members to hold a bar on one
//     session, and G3 forbids that at seed time.
//   - I3 needs the members' issuer codes to disagree, and G1 IS issuer-code
//     equality, so on the auto-accepted set it is a tautology. The fixture
//     below therefore gives the two companies the same NSDL issuer code,
//     which is what a G1-passing ticker reuse looks like.
//   - eod2 cannot supply the overlap either: eod2.LoadDir files a ticker's
//     whole CSV under today's ISIN, so predecessors have no eod2 bars at all
//     (verified on the live store: 0 of 572).
//
// So post-hoc detection from inside the store is NIL for a link that passed
// G1 and G4. The gates and one human review pass are not one of three
// defences; they are the whole defence. G6, the boundary close ratio, is a
// gate rather than a detector precisely because there is no detector to be
// had.
//
// This test fails if someone later claims the invariants detect a wrong merge
// in general. That is its entire job: pinning the blind spot so it stays
// visible after the design document is forgotten.
func TestEntityInvariantsCatchABogusLink_Disjoint(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()

	// Two genuinely different companies, sharing an issuer code and a ticker,
	// trading in sequence: DEADCO stops, and six weeks later NEWCO -- a
	// different business -- starts under the freed ticker.
	var sessions []time.Time
	for i := 0; i < 10; i++ {
		sessions = append(sessions, market.Day(2023, 5, 1).AddDate(0, 0, i))
	}
	var bars []market.Bar
	for i, d := range sessions {
		if i < 5 {
			bars = append(bars, barAt("INE999A01015", "SHELL", d, 12))
		} else {
			bars = append(bars, barAt("INE999A01023", "SHELL", d, 13))
		}
	}
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, bars)
	require.NoError(t, err)
	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE999A01015", Ticker: "SHELL", Date: sessions[0]},
		{ISIN: "INE999A01023", Ticker: "SHELL", Date: sessions[5]},
	})
	require.NoError(t, err)
	dead, live := ids["INE999A01015"], ids["INE999A01023"]

	linkRow(t, pool, live, dead, sessions[5])

	violations, err := store.CheckEntityInvariants(ctx, time.Now())
	require.NoError(t, err)
	require.Empty(t, violations,
		"the structural net cannot catch a disjoint wrong merge -- not the overlap check, not the issuer check, not ever")

	got, err := store.BarsForDate(ctx, market.SourceBhavcopy, sessions[7], time.Now())
	require.NoError(t, err, "there is no colliding bar to notice, because the merged company is dead")
	require.Len(t, got, 1)
	require.Equal(t, dead, got[0].EntityID)

	u, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, sessions[9], 10, 500, time.Now())
	require.NoError(t, err)
	m := memberOf(t, u.Members, "SHELL")
	require.Equal(t, dead, m.EntityID)
	require.Equal(t, 10, m.DaysPresent,
		"the wrong merge produces a member that looks exactly like a real one: ten sessions, two fragments, one plausible history")
	require.Equal(t, 2, m.Fragments)
	require.NotNil(t, m.LastBreak)
}

// TestEntityMapAt_ResolvesEverySymbolAtThePin covers the Go door onto the
// function §8.15 pins in SQL.
//
// `verdict entities check` has to answer "is this candidate pair already
// linked?" for every accepted candidate the generator produces, and the
// answer must be read at the caller's pin -- the monthly run checks the map
// as of a moment, and a check that resolved through entity_map_now would
// report a pair as unlinked (or as linked) on the strength of a row the
// pinned read cannot see. Routing it through the published entity_map_at
// rather than a fourth hand-rolled DISTINCT ON also means the CLI and a
// notebook resolve identity through one definition, which is the claim the
// notebook documentation makes.
func TestEntityMapAt_ResolvesEverySymbolAtThePin(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE081A01012", Ticker: "TATASTEEL", Date: market.Day(2015, 1, 2)},
		{ISIN: "INE081A01020", Ticker: "TATASTEEL", Date: market.Day(2022, 10, 31)},
		{ISIN: "INE002A01018", Ticker: "RELIANCE", Date: market.Day(2015, 1, 2)},
	})
	require.NoError(t, err)
	pred, succ, other := ids["INE081A01012"], ids["INE081A01020"], ids["INE002A01018"]

	before, err := store.EntityMapAt(ctx, time.Now())
	require.NoError(t, err)
	require.Equal(t, map[int64]int64{pred: pred, succ: succ, other: other}, before,
		"with no link rows every symbol is its own entity, and every registered symbol appears: an absent key would read as 'unlinked' by accident")

	t0 := time.Now()
	time.Sleep(10 * time.Millisecond)
	linkRow(t, store.Pool(), succ, pred, market.Day(2022, 7, 29))

	after, err := store.EntityMapAt(ctx, time.Now())
	require.NoError(t, err)
	require.Equal(t, map[int64]int64{pred: pred, succ: pred, other: other}, after)

	pinned, err := store.EntityMapAt(ctx, t0)
	require.NoError(t, err)
	require.Equal(t, succ, pinned[succ],
		"pinned before the link was ingested the successor is still its own entity; this is the ingested_at bound, and it is the easiest thing here to drop by accident")
}
