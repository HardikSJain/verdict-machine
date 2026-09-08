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

// The return fence, tested against a SEEDED map.
//
// The design is explicit that a green run of these against the live store
// would prove nothing on its own: a guard that never fires and a guard that
// is not there produce identical output. So every case here applies a real
// link to verdict_test and then asks for a return that must be refused.
//
// The fixture is TATASTEEL's actual 2022 split, with NSE's own dates and
// closes, because the shape that matters is not "a split" but "the ex-date
// falls one session BEFORE the ISIN changes" -- true of 262 of the 444 links
// in the live roster, and the case a fence built on the boundary alone gets
// wrong.

// tataSplit is the real thing: closes and dates as nse-bhavcopy holds them.
// The ISIN changes on the 29th; the price already collapsed on the 28th.
const (
	splitPredISIN = "INE081A01012"
	splitSuccISIN = "INE081A01020"
	preSplitClose = 950.00 // every predecessor session up to the 26th
	exEveClose    = 959.40 // 2022-07-27, the last session at the old level
	exDayClose    = 100.35 // 2022-07-28, ex-split, still the OLD symbol_id
	postClose     = 107.60 // 2022-07-29, the first session on the new ISIN
)

var (
	splitBoundary = market.Day(2022, 7, 29) // successor's first session
	// guardStart is the 5th session before the boundary on this fixture's
	// calendar: 28th, 27th, 26th, 25th, then the 22nd across the weekend.
	wantGuardStart = market.Day(2022, 7, 22)
)

// weekdaysBetween is the fixture's exchange calendar. It is every weekday in
// the range, holidays included, which is fine because the guard band is
// measured on whatever sessions the store holds -- the point of the test is
// the RULE, and a fixture that skipped Independence Day would only move the
// dates.
func weekdaysBetween(from, to time.Time) []time.Time {
	var out []time.Time
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		out = append(out, d)
	}
	return out
}

// seedSplitBars writes the symbols and the bars but NOT the link, so a test
// can pin either side of the moment the roster was applied.
func seedSplitBars(t *testing.T, store *market.Store, pool *pgxpool.Pool) (pred, succ int64) {
	t.Helper()
	ctx := context.Background()

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: splitPredISIN, Ticker: "TATASTEEL", Date: market.Day(2022, 1, 3)},
		{ISIN: splitSuccISIN, Ticker: "TATASTEEL", Date: market.Day(2022, 8, 1)},
	})
	require.NoError(t, err)
	pred, succ = ids[splitPredISIN], ids[splitSuccISIN]

	for _, d := range weekdaysBetween(market.Day(2022, 7, 1), market.Day(2022, 7, 28)) {
		close := preSplitClose
		switch {
		case d.Equal(market.Day(2022, 7, 27)):
			close = exEveClose
		case d.Equal(market.Day(2022, 7, 28)):
			close = exDayClose
		}
		insertBarDirect(t, pool, pred, "nse-bhavcopy", d, close)
	}
	for i, d := range weekdaysBetween(splitBoundary, market.Day(2022, 8, 31)) {
		insertBarDirect(t, pool, succ, "nse-bhavcopy", d, postClose+float64(i)*0.25)
	}
	return pred, succ
}

// seedSplit builds the whole TATASTEEL fixture and returns the entity id.
func seedSplit(t *testing.T, store *market.Store, pool *pgxpool.Pool) int64 {
	t.Helper()
	pred, succ := seedSplitBars(t, store, pool)
	linkRow(t, pool, succ, pred, splitBoundary)
	return pred
}

// TestEntityReturns_RefusesTheWindowThatEndsOnTheExDate is the case a fence
// built on the boundary alone gets wrong, and it is the majority case.
//
// The window 2022-07-01 .. 2022-07-28 never touches the ISIN change on the
// 29th. Both endpoints are the SAME symbol_id, so nothing about identity is
// crossed and every entity check in the repository is happy. What it crosses
// is the ex-date, and the number it would produce is -89.4%: not an outlier,
// not a flag, just a very good momentum score for a company whose share count
// went up tenfold.
//
// The assertion is deliberately made twice over: that the fence refuses, and
// that the two closes it refused to difference really do produce that number.
// The second half is what stops this test from passing after someone deletes
// the guard band and leaves the refusal firing for an unrelated reason.
func TestEntityReturns_RefusesTheWindowThatEndsOnTheExDate(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	entity := seedSplit(t, store, store.Pool())
	now := time.Now()

	from, to := market.Day(2022, 7, 1), market.Day(2022, 7, 28)
	rs, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{entity}, from, to, now)
	require.NoError(t, err)

	require.Empty(t, rs.Priced, "the ex-date window must not produce a number")
	require.Empty(t, rs.Absent)
	refusal, ok := rs.Refused[entity]
	require.True(t, ok, "an unadjusted window ending on the ex-split session must be refused")

	require.Equal(t, splitBoundary, refusal.Boundary.Date)
	require.Equal(t, wantGuardStart, refusal.GuardStart,
		"the guard band must reach back 5 sessions; the ex-date sits inside it")
	require.Equal(t, splitPredISIN, refusal.Boundary.PredecessorISIN)
	require.Equal(t, splitSuccISIN, refusal.Boundary.SuccessorISIN)
	require.Contains(t, refusal.Error(), "wrong by the split factor")

	// What was prevented. -89.4% is the number the engine would have ranked on.
	prevented := exDayClose/preSplitClose - 1
	require.InDelta(t, -0.8944, prevented, 0.0005,
		"if this delta ever changes, the fixture stopped being the real split")
}

// TestEntityReturns_RefusesAcrossTheBoundaryAndPricesEitherSideOfIt walks the
// four positions a window can take relative to one boundary. Two of them are
// refusals and two are ordinary returns, and a fence that refused all four
// would be just as broken as one that refused none -- momentum would have no
// names left.
func TestEntityReturns_RefusesAcrossTheBoundaryAndPricesEitherSideOfIt(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	entity := seedSplit(t, store, store.Pool())
	now := time.Now()

	cases := []struct {
		name     string
		from, to time.Time
		refused  bool
		why      string
	}{
		{
			name: "spans the boundary",
			from: market.Day(2022, 7, 1), to: market.Day(2022, 8, 31), refused: true,
			why: "old level to new level, the -90% the design names",
		},
		{
			name: "ends inside the guard band, before the boundary",
			from: market.Day(2022, 7, 1), to: market.Day(2022, 7, 26), refused: true,
			why: "conservative: the ex-date could have been any session in the band",
		},
		{
			name: "ends the session before the guard band opens",
			from: market.Day(2022, 7, 1), to: market.Day(2022, 7, 21), refused: false,
			why: "wholly pre-split, both closes at the old level",
		},
		{
			name: "starts on the boundary",
			from: splitBoundary, to: market.Day(2022, 8, 31), refused: false,
			why: "wholly post-split, both closes on the new ISIN",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{entity}, c.from, c.to, now)
			require.NoError(t, err)
			require.NoError(t, rs.Accounted([]int64{entity}))
			if c.refused {
				require.Contains(t, rs.Refused, entity, c.why)
				require.NotContains(t, rs.Priced, entity)
				return
			}
			priced, ok := rs.Priced[entity]
			require.True(t, ok, c.why)
			require.NotContains(t, rs.Refused, entity)
			require.True(t, priced.FromDate.Before(priced.ToDate))
			require.InDelta(t, priced.ToClose/priced.FromClose-1, priced.Return, 1e-12)
			require.Less(t, priced.Return, 0.5,
				"neither safe window may produce a split-sized return")
			require.Greater(t, priced.Return, -0.5,
				"neither safe window may produce a split-sized return")
		})
	}
}

// TestEntityReturns_ProtectionIsContingentOnThePinnedRoster states the limit
// of this whole fence, and it is not a comfortable one.
//
// The ex-date step lives INSIDE the predecessor's own symbol_id: 950.00 on the
// 26th, 100.35 on the 28th, one instrument throughout. Nothing about identity
// is crossed there, so the only reason the fence sees it at all is that a
// nearby boundary exists in the map AT THE CALLER'S PIN. Pin before the roster
// was applied and the very same window prices at -89.4% with nothing to
// object.
//
// That is the design's "replay reproduces the answer, not the judgement" made
// executable. A pre-roster run replays exactly as it ran, wrong number and
// all, and no code in this repository can or should change that. What it
// means in practice is narrower and worth stating: the fence protects runs
// pinned at or after the apply, which is every run M1 will make, and it is
// not retroactive.
func TestEntityReturns_ProtectionIsContingentOnThePinnedRoster(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()

	pred, succ := seedSplitBars(t, store, pool)
	beforeLink := time.Now()
	time.Sleep(10 * time.Millisecond)
	linkRow(t, pool, succ, pred, splitBoundary)

	// The window that ends on the ex-date: one symbol, both endpoints.
	from, to := market.Day(2022, 7, 1), market.Day(2022, 7, 28)

	early, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{pred}, from, to, beforeLink)
	require.NoError(t, err)
	require.Empty(t, early.Refused, "pinned before the roster there is no boundary to see")
	priced, ok := early.Priced[pred]
	require.True(t, ok, "and the bars were already there, so it prices")
	require.InDelta(t, -0.8944, priced.Return, 0.0005,
		"this is the number a pre-roster replay reproduces, and it is wrong by 10x")

	late, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{pred}, from, to, time.Now())
	require.NoError(t, err)
	require.Contains(t, late.Refused, pred,
		"the same bars, the same window, pinned after the apply: refused")
	require.Empty(t, late.Priced)

	// The endpoint query's own pin, isolated. At beforeLink the successor is
	// still a separate entity, so the predecessor's series ENDS on the 28th
	// and a window reaching to 2022-08-31 has no live endpoint. Drop the
	// ingested_at bound out of the endpoint query's entity map and this
	// becomes a priced -89% instead, because the successor's August bars start
	// answering for an entity that did not exist at the pin. The boundary set
	// carries its own pin and would not object: it is looking at a different
	// question.
	wide, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{pred},
		market.Day(2022, 7, 1), market.Day(2022, 8, 31), beforeLink)
	require.NoError(t, err)
	require.Contains(t, wide.Absent, pred,
		"an unpinned entity map here would price the successor's bars into an entity that had no members yet")
	require.Empty(t, wide.Priced)
	require.Empty(t, wide.Refused)
}

// TestEntityReturns_AccountsForEveryRequestedEntity is the invariant that
// makes a refusal impossible to lose by accident. A caller that only reads
// Priced gets a shorter list than it asked for; this is what lets it notice.
func TestEntityReturns_AccountsForEveryRequestedEntity(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()
	entity := seedSplit(t, store, pool)

	// A live name with no succession, so the set has a priced member too.
	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE002A01018", Ticker: "RELIANCE", Date: market.Day(2022, 7, 1)},
	})
	require.NoError(t, err)
	clean := ids["INE002A01018"]
	for i, d := range weekdaysBetween(market.Day(2022, 7, 1), market.Day(2022, 8, 31)) {
		insertBarDirect(t, pool, clean, "nse-bhavcopy", d, 2500+float64(i))
	}

	const neverTraded = int64(987654)
	requested := []int64{entity, clean, neverTraded}

	rs, err := store.EntityReturns(ctx, "nse-bhavcopy", requested,
		market.Day(2022, 7, 1), market.Day(2022, 8, 31), time.Now())
	require.NoError(t, err)
	require.NoError(t, rs.Accounted(requested))

	require.Contains(t, rs.Priced, clean)
	require.Contains(t, rs.Refused, entity)
	require.Contains(t, rs.Absent, neverTraded)
	require.Len(t, rs.Priced, 1)
	require.Len(t, rs.Refused, 1)
	require.Len(t, rs.Absent, 1)

	// Duplicates in the request are not three answers for one name.
	dup, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{clean, clean, clean},
		market.Day(2022, 7, 1), market.Day(2022, 8, 31), time.Now())
	require.NoError(t, err)
	require.Len(t, dup.Priced, 1)
	require.NoError(t, dup.Accounted([]int64{clean}))
}

// TestEntityReturns_StaleEndpointIsAbsentNotAReturn pins the floor under "the
// last session at or before X". Without it a name that stopped trading in July
// would answer a September window with a July close, and the resulting number
// would be a real return over a period the name did not trade.
func TestEntityReturns_StaleEndpointIsAbsentNotAReturn(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	pool := store.Pool()

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE733E01010", Ticker: "GONEQUIET", Date: market.Day(2022, 7, 1)},
	})
	require.NoError(t, err)
	dead := ids["INE733E01010"]
	for _, d := range weekdaysBetween(market.Day(2022, 7, 1), market.Day(2022, 7, 29)) {
		insertBarDirect(t, pool, dead, "nse-bhavcopy", d, 300)
	}

	rs, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{dead},
		market.Day(2022, 7, 4), market.Day(2022, 12, 30), time.Now())
	require.NoError(t, err)
	require.Contains(t, rs.Absent, dead)
	require.Empty(t, rs.Priced)

	// Inside the staleness window it still answers, which is what makes the
	// case above a floor rather than a blanket refusal of quiet names.
	live, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{dead},
		market.Day(2022, 7, 4), market.Day(2022, 8, 10), time.Now())
	require.NoError(t, err)
	require.Contains(t, live.Priced, dead)
	require.Equal(t, market.Day(2022, 7, 29), live.Priced[dead].ToDate)
}

// TestEntityReturns_RejectsAnInvertedWindow: from must precede to. A caller
// that swapped them would otherwise get a silently reciprocal return.
func TestEntityReturns_RejectsAnInvertedWindow(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	entity := seedSplit(t, store, store.Pool())

	_, err := store.EntityReturns(ctx, "nse-bhavcopy", []int64{entity},
		market.Day(2022, 8, 31), market.Day(2022, 7, 1), time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be before")
}

// TestReturnSetAccountedCatchesABookkeepingSlip exercises Accounted on its
// own, because every other test here calls it on a set that is already
// correct and so cannot show that it would object.
func TestReturnSetAccountedCatchesABookkeepingSlip(t *testing.T) {
	rs := market.ReturnSet{
		Priced:  map[int64]market.EntityReturn{1: {EntityID: 1}},
		Refused: map[int64]market.BoundaryRefusal{},
		Absent:  map[int64]string{},
	}
	require.NoError(t, rs.Accounted([]int64{1}))

	err := rs.Accounted([]int64{1, 2})
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 unaccounted [2]")

	rs.Refused[1] = market.BoundaryRefusal{EntityID: 1}
	require.Error(t, rs.Accounted([]int64{1}), "an id in two maps is counted twice")

	rs = market.ReturnSet{
		Priced:  map[int64]market.EntityReturn{9: {EntityID: 9}},
		Refused: map[int64]market.BoundaryRefusal{},
		Absent:  map[int64]string{},
	}
	err = rs.Accounted([]int64{1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unrequested [9]")
}
