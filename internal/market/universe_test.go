package market_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/bhavcopy"
	"github.com/HardikSJain/verdict-machine/internal/market/eod2"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

func loadBhavcopyFixture(t *testing.T, name string) []market.Bar {
	t.Helper()
	f, err := os.Open("bhavcopy/testdata/" + name)
	require.NoError(t, err)
	defer f.Close()
	bars, err := bhavcopy.Parse(name, f)
	require.NoError(t, err)
	return bars
}

func tickers(members []market.UniverseMember) map[string]bool {
	set := map[string]bool{}
	for _, m := range members {
		set[m.Ticker] = true
	}
	return set
}

func TestUniverseAsOf_2015ContainsNamesDelistedSince(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	for _, name := range []string{"cm29JUN2015bhav.csv", "cm30JUN2015bhav.csv"} {
		_, err := store.InsertBars(ctx, market.SourceBhavcopy, loadBhavcopyFixture(t, name))
		require.NoError(t, err)
	}

	u, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2015, 6, 30), 2, 500, time.Now())
	require.NoError(t, err)
	members := u.Members
	require.Len(t, members, 500)
	require.Equal(t, 2, u.Sessions, "two fixture sessions were loaded, so the window is two sessions wide")
	require.GreaterOrEqual(t, members[0].MedianTurnover, members[499].MedianTurnover, "sorted by median turnover, descending")
	require.Equal(t, 2, members[0].DaysPresent)

	got := tickers(members)
	require.True(t, got["DHFL"], "DHFL delisted 2021; a survivor-only universe would miss it")
	require.True(t, got["JETAIRWAYS"], "Jet Airways delisted; a survivor-only universe would miss it")
	require.True(t, got["TATASTEEL"])
}

func TestUniverseAsOf_RequiresPresenceOn80PctOfDays(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	day1 := loadBhavcopyFixture(t, "cm29JUN2015bhav.csv")
	day2 := loadBhavcopyFixture(t, "cm30JUN2015bhav.csv")
	// Drop DHFL from one of the two days; with a 2-day window that is 50% presence.
	var trimmed []market.Bar
	for _, b := range day2 {
		if b.Ticker != "DHFL" {
			trimmed = append(trimmed, b)
		}
	}
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, day1)
	require.NoError(t, err)
	_, err = store.InsertBars(ctx, market.SourceBhavcopy, trimmed)
	require.NoError(t, err)

	u, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2015, 6, 30), 2, 500, time.Now())
	require.NoError(t, err)
	require.False(t, tickers(u.Members)["DHFL"])
}

// TestUniverseAsOf_AsOfIngestIsolatesRevisions is not in the brief's Step 1;
// it is added because the brief's three tests all pass time.Now() as
// asOfIngest and so never exercise UniverseAsOf's own point-in-time
// parameter. The `ingested_at <= $5` predicate is the entire reason this
// project keeps an insert-only, versioned bars table, so it is asserted here
// against UniverseAsOf directly rather than assumed from the existing
// coverage of the same predicate on BarsForDate (store_test.go).
func TestUniverseAsOf_AsOfIngestIsolatesRevisions(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	bars := loadBhavcopyFixture(t, "cm29JUN2015bhav.csv")
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, bars)
	require.NoError(t, err)

	firstIngest := time.Now()
	time.Sleep(20 * time.Millisecond)

	var original float64
	revised := make([]market.Bar, len(bars))
	copy(revised, bars)
	found := false
	for i, b := range revised {
		if b.Ticker == "RELIANCE" {
			original = *b.Turnover
			doubled := original * 2
			revised[i].Turnover = &doubled
			found = true
		}
	}
	require.True(t, found, "fixture must contain RELIANCE")
	n, err := store.InsertBars(ctx, market.SourceBhavcopy, revised)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only RELIANCE's turnover changed")

	before, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2015, 6, 29), 1, 500, firstIngest)
	require.NoError(t, err)
	after, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2015, 6, 29), 1, 500, time.Now())
	require.NoError(t, err)

	require.Equal(t, original, medianOf(t, before.Members, "RELIANCE"), "as-of a timestamp before the revision returns the original value")
	require.Equal(t, original*2, medianOf(t, after.Members, "RELIANCE"), "as-of now returns the revised value")

	var versions int
	require.NoError(t, store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM bars b JOIN symbols s ON s.symbol_id = b.symbol_id
		 WHERE s.isin = $1 AND b.date = $2 AND b.source = $3`,
		"INE002A01018", market.Day(2015, 6, 29), market.SourceBhavcopy).Scan(&versions))
	require.Equal(t, 2, versions, "both versions are retained; the as-of predicate reads, it never rewrites")
}

func medianOf(t *testing.T, members []market.UniverseMember, ticker string) float64 {
	t.Helper()
	for _, m := range members {
		if m.Ticker == ticker {
			return m.MedianTurnover
		}
	}
	t.Fatalf("%s not found in universe", ticker)
	return 0
}

func TestUniverseAsOf_Eod2SourceIsSurvivorOnly(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	f, err := os.Open("eod2/testdata/daily/tatasteel.csv")
	require.NoError(t, err)
	defer f.Close()
	bars, err := eod2.ParseDaily(f, "TATASTEEL", "INE081A01020")
	require.NoError(t, err)
	_, err = store.InsertBars(ctx, market.SourceEod2, bars)
	require.NoError(t, err)

	// The fixture holds 13 sessions ending 2022-08-05, so lookbackDays is the
	// only thing that decides how wide the window is here. Asserting the
	// realised window and DaysPresent for two different lookbacks is what
	// pins the `LIMIT $5` in the days CTE: widen, narrow or drop it and one
	// of these two calls stops matching. Without them the parameter that
	// defines the universe -- 125 sessions of median turnover -- had no test
	// that could fail.
	five, err := store.UniverseAsOf(ctx, market.SourceEod2, market.Day(2022, 8, 5), 5, 500, time.Now())
	require.NoError(t, err)
	require.Len(t, five.Members, 1)
	require.Equal(t, "TATASTEEL", five.Members[0].Ticker)
	require.Greater(t, five.Members[0].MedianTurnover, 0.0, "eod2 has no turnover column; close*volume stands in")
	require.Equal(t, 5, five.Sessions)
	require.Equal(t, 5, five.Members[0].DaysPresent, "a 5-session lookback ranks on exactly 5 sessions")

	all, err := store.UniverseAsOf(ctx, market.SourceEod2, market.Day(2022, 8, 5), 13, 500, time.Now())
	require.NoError(t, err)
	require.Len(t, all.Members, 1)
	require.Equal(t, 13, all.Sessions)
	require.Equal(t, 13, all.Members[0].DaysPresent, "a 13-session lookback ranks on all 13")
	require.NotEqual(t, five.Members[0].MedianTurnover, all.Members[0].MedianTurnover,
		"a wider window is a different median; if these match, the lookback is not being applied")

	// A lookback longer than the store holds shortens to what is there
	// instead of erroring or pretending, and says so.
	wide, err := store.UniverseAsOf(ctx, market.SourceEod2, market.Day(2022, 8, 5), 5000, 500, time.Now())
	require.NoError(t, err)
	require.Equal(t, 13, wide.Sessions, "the realised window is the whole history, not the 5000 asked for")
}

// TestUniverseAsOf_RejectsUnknownSource is not in the brief's Step 1; it is
// added for a fix-round-1 finding: source is a free-form string with no
// allow-list anywhere in the brief's given code, so a typo of "nse-bhavcopy"
// or "eod2" (e.g. from the CLI's --source flag) silently matched zero rows
// and returned an empty, err == nil universe -- indistinguishable from "no
// symbols qualify this window". No bars are inserted here: the check must
// reject before ever querying, so an empty store still proves it.
func TestUniverseAsOf_RejectsUnknownSource(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	u, err := store.UniverseAsOf(ctx, "bogus", market.Day(2015, 6, 30), 2, 500, time.Now())
	require.ErrorContains(t, err, `unknown source "bogus"`)
	require.Empty(t, u.Members)
}

// TestUniverseAsOf_TickerIsPointInTimeOnBothAxes mirrors store_test.go's two
// label tests against UniverseAsOf, which resolves the label through the same
// lateral join and had the same two gaps: replacing its
// `symbols.ingested_at <= $4` with `(... OR true)` left the suite green, and
// nothing asserted that a 2015 universe names symbols the way 2015 did.
// M1's evening alerts print these tickers to a human placing real orders.
func TestUniverseAsOf_TickerIsPointInTimeOnBothAxes(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	const isin = "INE095I01015"
	old2015 := market.Day(2015, 6, 30)
	new2016 := market.Day(2016, 6, 30)

	_, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{barAt(isin, "SONASTEER", old2015, 100)})
	require.NoError(t, err)
	_, err = store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{barAt(isin, "JTEKTINDIA", new2016, 120)})
	require.NoError(t, err)

	tickerAsOf := func(asOf, asOfIngest time.Time) string {
		u, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, asOf, 1, 10, asOfIngest)
		require.NoError(t, err)
		require.Len(t, u.Members, 1)
		return u.Members[0].Ticker
	}
	require.Equal(t, "SONASTEER", tickerAsOf(old2015, time.Now()), "the 2015 universe names the 2015 ticker")
	require.Equal(t, "JTEKTINDIA", tickerAsOf(new2016, time.Now()))

	secondIngest := time.Now()
	time.Sleep(20 * time.Millisecond)
	// A correction to what that 2015 session called the symbol, written today.
	_, err = store.EnsureSymbols(ctx, []market.Bar{{ISIN: isin, Ticker: "SONACOMS", Date: old2015}})
	require.NoError(t, err)

	require.Equal(t, "SONASTEER", tickerAsOf(old2015, secondIngest),
		"as-of an ingest timestamp before the correction, the label is the one the store held then")
	require.Equal(t, "SONACOMS", tickerAsOf(old2015, time.Now()))
	require.Equal(t, "JTEKTINDIA", tickerAsOf(new2016, time.Now()), "correcting 2015 does not relabel 2016")
}

// TestUniverseAsOf_EmptyRankingStillReportsTheWindow pins the case that made
// the realised session count and the ranking share one statement: a window
// that is real but ranks nobody. A caller handed zero members needs the
// window most of all, because that is the only thing that separates "the
// store holds these sessions and nothing in them qualified" from "the store
// holds nothing here at all" -- and the two call for opposite responses, one
// a filter to loosen and the other a backfill to run.
//
// The bars below are series BE, so the window's sessions exist but
// window_rows (series = 'EQ') is empty. Collapsing the two queries into one
// is what put this at risk: a plain inner join to the ranking would return no
// rows at all and take the session count down with it.
func TestUniverseAsOf_EmptyRankingStillReportsTheWindow(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	var bars []market.Bar
	for _, d := range []time.Time{market.Day(2015, 6, 29), market.Day(2015, 6, 30)} {
		b := barAt("INE081A01020", "TATASTEEL", d, 100)
		b.Series = "BE"
		bars = append(bars, b)
	}
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, bars)
	require.NoError(t, err)

	u, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2015, 6, 30), 2, 500, time.Now())
	require.NoError(t, err)
	require.Empty(t, u.Members, "no EQ series in the window, so nobody is ranked")
	require.Equal(t, 2, u.Sessions, "the window is still two real sessions and must say so")

	// A store with nothing in range reports a zero window, which is the state
	// the empty ranking above has to stay distinguishable from.
	none, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2001, 6, 30), 2, 500, time.Now())
	require.NoError(t, err)
	require.Empty(t, none.Members)
	require.Equal(t, 0, none.Sessions)
}
