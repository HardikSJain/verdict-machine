package entities_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/entities"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// Design 8.2, 8.3, 8.4 and 8.10: the succession fixture, exercised through
// the real writer.
//
// These live beside the seeder rather than beside the reader on purpose. The
// links are written by `entities apply` and undone by `entities retract`, not
// by a hand-built INSERT, so what they pin is the whole path a link actually
// travels: roster -> gates -> digest -> rows -> read. Design 8.4 is explicit
// that a version of the retraction test asserting on a pre-built fixture
// would have gone green over an undo that could not run.

const (
	predISIN = "INE081A01012" // TATASTEEL until 2022-07-28
	succISIN = "INE081A01020" // TATASTEEL from 2022-07-29, after the 1:10 split
	ctrlISIN = "INE666D01014" // a control name that never changed ISIN
)

// boundary is the real TATASTEEL succession date: the successor's first
// nse-bhavcopy session.
var boundary = market.Day(2022, 7, 29)

// asOf2022 is the window end design 8.2 names, chosen because it is where the
// live archive shows the damage: neither half of the split clears the 80%
// presence gate for the whole window around the boundary.
var asOf2022 = market.Day(2022, 10, 31)

// sessionsEndingAt returns n weekday sessions ending at end, oldest first.
// The fixture uses weekdays rather than NSE's real calendar because what is
// under test is the entity rule, not the holiday list; the consequence is
// that the fragment counts below are 58/67 where the live archive (with its
// holidays) gives 62/63. Neither clears 100, which is the point.
func sessionsEndingAt(end time.Time, n int) []time.Time {
	var out []time.Time
	for d := end; len(out) < n; d = d.AddDate(0, 0, -1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		out = append(out, d)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func bar(isin, ticker string, d time.Time, close float64, turnover float64) market.Bar {
	return market.Bar{
		ISIN: isin, Ticker: ticker, Series: "EQ", Date: d,
		Open: close, High: close, Low: close, Close: close,
		Volume: 1000, Turnover: &turnover,
	}
}

// tataSteelFixture builds the two halves of a split company plus a control
// name that never changed ISIN, over a weekday calendar running from 2014 to
// 2022-10-31. The predecessor's history reaches back far enough that the same
// entity can be asked about at asOf 2015-01-02, so the date axis is exercised
// in both directions rather than at one point.
func tataSteelFixture(t *testing.T) (*market.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.Pool(t)
	store := market.NewStore(pool)
	ctx := context.Background()

	var bars []market.Bar
	for _, d := range sessionsEndingAt(asOf2022, 2200) {
		bars = append(bars, bar(ctrlISIN, "CONTROL", d, 100, 5_000_000))
		if d.Before(boundary) {
			bars = append(bars, bar(predISIN, "TATASTEEL", d, 410.75, 9_000_000))
		} else {
			bars = append(bars, bar(succISIN, "TATASTEEL", d, 107.60, 9_000_000))
		}
	}
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, bars)
	require.NoError(t, err)
	return store, pool
}

// tataSteelRoster is the one-line roster for the fixture, carrying the gate
// results a propose run would have measured on it.
func tataSteelRoster(t *testing.T) (*entities.Roster, []byte) {
	t.Helper()
	l := tataSteel()
	l.Gates.PredecessorLastBar = "2022-07-28"
	l.Gates.SuccessorFirstBar = "2022-07-29"
	l.EffectiveFrom = "2022-07-29"
	r := roster(l)
	d, err := r.ComputeDigest()
	require.NoError(t, err)
	return r, d
}

func memberByTicker(u market.Universe, ticker string) (market.UniverseMember, bool) {
	for _, m := range u.Members {
		if m.Ticker == ticker {
			return m, true
		}
	}
	return market.UniverseMember{}, false
}

// TestSuccessionRepairsTataSteelWindow is design 8.2: the large-cap proof.
func TestSuccessionRepairsTataSteelWindow(t *testing.T) {
	ctx := context.Background()
	store, pool := tataSteelFixture(t)

	// Before the link: two disjoint symbols, neither of which can clear a
	// gate that asks for 100 of 125 sessions. This is the live defect --
	// India's 28th most traded name absent from its own point-in-time
	// universe for six months, then reappearing as a stranger with no
	// history.
	u, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, asOf2022, 125, 500, dbNow(t, pool))
	require.NoError(t, err)
	require.Equal(t, 125, u.Sessions)
	_, ok := memberByTicker(u, "TATASTEEL")
	require.False(t, ok, "neither fragment clears the 80% presence gate, so the name is simply absent")

	// The fragments, counted off bars directly -- neither is a MEMBER of any
	// universe over this window, under any option, so there is no
	// days_present to read off a row. That is the defect: not a bad ranking,
	// an absence.
	pred := sessionsInWindow(t, pool, predISIN, asOf2022, 125)
	succ := sessionsInWindow(t, pool, succISIN, asOf2022, 125)
	require.Equal(t, 58, pred, "the pre-split half holds 58 of 125 sessions where ceil(0.8*125) = 100 are needed")
	require.Equal(t, 67, succ, "the post-split half holds 67 of 125 sessions where 100 are needed")
	require.Equal(t, 125, pred+succ, "together they are the whole window, and separately neither is anything")

	// Link them through the real writer.
	r, digest := tataSteelRoster(t)
	res, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)

	u, err = store.UniverseAsOf(ctx, market.SourceBhavcopy, asOf2022, 125, 500, dbNow(t, pool))
	require.NoError(t, err)
	m, ok := memberByTicker(u, "TATASTEEL")
	require.True(t, ok, "the entity holds the whole window and ranks where it belongs")
	require.Equal(t, 125, m.DaysPresent, "presence is counted in distinct SESSIONS across the entity's members")
	require.Equal(t, 2, m.Fragments, "two physical symbol_ids contributed, so SymbolID is not a safe join key for the window")
	require.Equal(t, succISIN, m.ISIN, "the label is the member in force on asOf, chosen by the boundary interval")
	require.NotNil(t, m.LastBreak)
	require.Equal(t, boundary, *m.LastBreak,
		"a return computed across this date on unadjusted bhavcopy prices is wrong by the split factor, and LastBreak is how M1 finds out")

	// The other direction on the date axis: the same entity, asked about
	// before the boundary, is its predecessor.
	u, err = store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2015, 1, 2), 125, 500, dbNow(t, pool))
	require.NoError(t, err)
	old, ok := memberByTicker(u, "TATASTEEL")
	require.True(t, ok)
	require.Equal(t, predISIN, old.ISIN, "on a 2015 session the entity really was INE081A01012")
	require.Equal(t, m.EntityID, old.EntityID, "and it is the same entity, under the same id, in both directions")
	require.Nil(t, old.LastBreak, "no boundary has happened yet at asOf 2015-01-02")
}

// TestSuccessionIsInvisibleBeforeItsIngest is design 8.3: the replay proof.
func TestSuccessionIsInvisibleBeforeItsIngest(t *testing.T) {
	ctx := context.Background()
	store, pool := tataSteelFixture(t)

	t0 := dbNow(t, pool)
	before, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, asOf2022, 125, 500, t0)
	require.NoError(t, err)
	require.Len(t, before.Members, 1, "only the control name clears the gate before the seed")

	r, digest := tataSteelRoster(t)
	_, err = entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
	t1 := dbNow(t, pool)

	// The links inserted at t1 are not compensated for in a query pinned at
	// t0; they are literally invisible to it. Every answer this system has
	// ever given replays byte-identically after the seed, including the ones
	// that were wrong about TATASTEEL -- which is correct, because replay's
	// job is to reproduce what we believed then.
	after, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, asOf2022, 125, 500, t0)
	require.NoError(t, err)
	require.Equal(t, before, after, "a run pinned before the seed must answer identically forever")

	now, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, asOf2022, 125, 500, t1)
	require.NoError(t, err)
	m, ok := memberByTicker(now, "TATASTEEL")
	require.True(t, ok)
	require.Equal(t, 125, m.DaysPresent)
}

// TestRetractionIsInvisibleBeforeItsIngest is design 8.4. It EXECUTES every
// retraction it describes, because the point of the test is that the inserts
// themselves succeed: revision 1's flatness trigger rejected the linked-member
// retraction outright and made the re-link permanently impossible, and a
// version of this test asserting on a pre-built fixture would have gone green
// over an undo that could not run.
func TestRetractionIsInvisibleBeforeItsIngest(t *testing.T) {
	ctx := context.Background()
	store, pool := tataSteelFixture(t)
	r, digest := tataSteelRoster(t)

	merged := func(pin time.Time) bool {
		u, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, asOf2022, 125, 500, pin)
		require.NoError(t, err)
		m, ok := memberByTicker(u, "TATASTEEL")
		return ok && m.DaysPresent == 125
	}
	ids := symbolIDs(t, pool)
	root, member := ids[predISIN], ids[succISIN]

	t0 := dbNow(t, pool)
	require.False(t, merged(t0), "before the link the window is fragmented")

	_, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
	t1 := dbNow(t, pool)
	require.True(t, merged(t1))

	// Naming the ROOT must dissolve the entity. Revision 1's symbol-scoped
	// retract inserted a row here, reported success, and left the merge
	// standing, because the member's own latest row still pointed at the
	// root.
	res, err := entities.Retract(ctx, pool, itoa(root), "false positive", "", false)
	require.NoError(t, err, "the retraction INSERT itself must succeed")
	require.Equal(t, root, res.EntityID)
	require.Len(t, res.Members, 1)
	require.Equal(t, member, res.Members[0].SymbolID, "the member is what gets a row; the root already resolves to itself")
	t2 := dbNow(t, pool)
	require.False(t, merged(t2), "from t2 onward the merge is gone")

	// All three windows stay reachable at their own timestamps.
	require.False(t, merged(t0))
	require.True(t, merged(t1), "the merged answer is still reproducible at the pin where it was given")

	// Re-linking after a retraction must work. Revision 1's second trigger
	// check rejected this permanently, which made the undo one-way and put
	// the corrected answer out of reach.
	_, err = entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err, "a retraction that cannot be undone is a one-way door")
	t3 := dbNow(t, pool)
	require.True(t, merged(t3))

	// --symbol on a root that still has members refuses, and names them.
	_, err = entities.Retract(ctx, pool, itoa(root), "", "", true)
	require.ErrorContains(t, err, "ROOT")
	require.ErrorContains(t, err, itoa(member), "the operator has to be told which members they meant")
	require.True(t, merged(dbNow(t, pool)), "a refusal changes nothing")

	// --symbol on the linked member is the single-member case and works.
	res, err = entities.Retract(ctx, pool, itoa(member), "second thoughts", "", true)
	require.NoError(t, err)
	require.Len(t, res.Members, 1)
	require.False(t, merged(dbNow(t, pool)))
}

// TestEod2AndBhavcopyShareAnIdentityButNotAPrice is design 8.10.
func TestEod2AndBhavcopyShareAnIdentityButNotAPrice(t *testing.T) {
	ctx := context.Background()
	store, pool := tataSteelFixture(t)
	preBoundary := market.Day(2015, 1, 2)

	// eod2.LoadDir maps a ticker's whole CSV to the ticker's CURRENT ISIN, so
	// an eod2 bar for a 2015 session is physically filed under the post-2022
	// symbol. Its close is adjusted in place; the bhavcopy close for the same
	// session is not, and the two differ by exactly the 1:10 factor.
	_, err := store.InsertBars(ctx, market.SourceEod2, []market.Bar{
		bar(succISIN, "TATASTEEL", preBoundary, 41.075, 900_000),
	})
	require.NoError(t, err)

	t0 := dbNow(t, pool)
	eodBefore := oneBar(t, store, market.SourceEod2, preBoundary, t0)
	bhavBefore := oneBar(t, store, market.SourceBhavcopy, preBoundary, t0)
	require.NotEqual(t, eodBefore.ISIN, bhavBefore.ISIN,
		"pinned before the seed the two sources disagree about identity: eod2 says today's ISIN, bhavcopy says the session's")

	r, digest := tataSteelRoster(t)
	_, err = entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
	t1 := dbNow(t, pool)

	eod := oneBar(t, store, market.SourceEod2, preBoundary, t1)
	bhav := oneBar(t, store, market.SourceBhavcopy, preBoundary, t1)
	require.Equal(t, bhav.EntityID, eod.EntityID, "one company")
	require.Equal(t, predISIN, eod.ISIN, "on that session the ISIN really was INE081A01012, whatever row the bar is filed under")
	require.Equal(t, predISIN, bhav.ISIN)
	require.Equal(t, bhav.Ticker, eod.Ticker)
	require.NotEqual(t, eod.SymbolID, bhav.SymbolID, "the identity is restored at read time; the rows are untouched")

	// And the price half, which is the part that must NOT be quietly
	// reconciled. The identity restored is identity only: until the read-time
	// adjustments layer exists, a cross-source price comparator built on this
	// would read the split factor as a data error.
	require.NotEqual(t, eod.Close, bhav.Close)
	require.InDelta(t, 10.0, bhav.Close/eod.Close, 1e-9, "the disagreement is exactly the 1:10 face value split")
}

func oneBar(t *testing.T, store *market.Store, source string, date, pin time.Time) market.StoredBar {
	t.Helper()
	bars, err := store.BarsForDate(context.Background(), source, date, pin)
	require.NoError(t, err)
	for _, b := range bars {
		if b.Ticker == "TATASTEEL" {
			return b
		}
	}
	t.Fatalf("no TATASTEEL bar for %s on %s", source, date.Format("2006-01-02"))
	return market.StoredBar{}
}

// sessionsInWindow counts the distinct sessions one ISIN traded on inside the
// last `lookback` sessions ending at asOf, read off bars rather than off a
// universe row -- a fragment that cannot clear the presence gate has no
// universe row to read.
// dbNow reads the database's clock, which is the clock symbol_links rows are
// stamped with. Taking a pin off the Go process's clock instead makes these
// tests hostage to sub-second skew between the test host and Postgres: a
// retraction written a few milliseconds after a pin can land BEFORE it, and
// the test then reports a replay defect that is really a clock difference.
func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var ts time.Time
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT clock_timestamp()").Scan(&ts))
	return ts
}

func sessionsInWindow(t *testing.T, pool *pgxpool.Pool, isin string, asOf time.Time, lookback int) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `
		WITH days AS (
			SELECT DISTINCT date FROM bars
			WHERE source = 'nse-bhavcopy' AND date <= $2
			ORDER BY date DESC LIMIT $3
		)
		SELECT count(DISTINCT b.date)::int
		FROM bars b
		JOIN symbols s ON s.symbol_id = b.symbol_id AND s.isin = $1
		JOIN days d ON d.date = b.date
		WHERE b.source = 'nse-bhavcopy'`, isin, asOf, lookback).Scan(&n)
	require.NoError(t, err)
	return n
}

func symbolIDs(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT DISTINCT isin, symbol_id FROM symbols`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var isin string
		var id int64
		require.NoError(t, rows.Scan(&isin, &id))
		out[isin] = id
	}
	require.NoError(t, rows.Err())
	return out
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
