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

	members, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2015, 6, 30), 2, 500, time.Now())
	require.NoError(t, err)
	require.Len(t, members, 500)
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

	members, err := store.UniverseAsOf(ctx, market.SourceBhavcopy, market.Day(2015, 6, 30), 2, 500, time.Now())
	require.NoError(t, err)
	require.False(t, tickers(members)["DHFL"])
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

	require.Equal(t, original, medianOf(t, before, "RELIANCE"), "as-of a timestamp before the revision returns the original value")
	require.Equal(t, original*2, medianOf(t, after, "RELIANCE"), "as-of now returns the revised value")

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

	members, err := store.UniverseAsOf(ctx, market.SourceEod2, market.Day(2022, 8, 5), 5, 500, time.Now())
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "TATASTEEL", members[0].Ticker)
	require.Greater(t, members[0].MedianTurnover, 0.0, "eod2 has no turnover column; close*volume stands in")
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

	members, err := store.UniverseAsOf(ctx, "bogus", market.Day(2015, 6, 30), 2, 500, time.Now())
	require.ErrorContains(t, err, `unknown source "bogus"`)
	require.Nil(t, members)
}
