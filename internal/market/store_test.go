package market_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

func sampleBars() []market.Bar {
	turnover := 215046652.2
	return []market.Bar{
		{ISIN: "INE202B01012", Ticker: "DHFL", Series: "EQ", Date: market.Day(2015, 6, 30),
			Open: 421, High: 424.15, Low: 414.95, Close: 420.95, Volume: 512129, Turnover: &turnover},
		{ISIN: "INE081A01020", Ticker: "TATASTEEL", Series: "EQ", Date: market.Day(2015, 6, 30),
			Open: 300, High: 305, Low: 298, Close: 301.5, Volume: 1000000},
	}
}

func TestInsertBars_IsIdempotentAndVersioned(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	n, err := store.InsertBars(ctx, market.SourceBhavcopy, sampleBars())
	require.NoError(t, err)
	require.Equal(t, 2, n)

	n, err = store.InsertBars(ctx, market.SourceBhavcopy, sampleBars())
	require.NoError(t, err)
	require.Equal(t, 0, n, "identical content must not create a new version")

	firstIngest := time.Now()
	time.Sleep(20 * time.Millisecond)

	revised := sampleBars()
	revised[0].Close = 421.00 // NSE re-issued the file with a corrected close
	n, err = store.InsertBars(ctx, market.SourceBhavcopy, revised)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the changed bar gets a new version")

	before, err := store.BarsForDate(ctx, market.SourceBhavcopy, market.Day(2015, 6, 30), firstIngest)
	require.NoError(t, err)
	after, err := store.BarsForDate(ctx, market.SourceBhavcopy, market.Day(2015, 6, 30), time.Now())
	require.NoError(t, err)
	require.Len(t, before, 2)
	require.Len(t, after, 2)
	require.Equal(t, 420.95, closeOf(before, "DHFL"))
	require.Equal(t, 421.00, closeOf(after, "DHFL"))
	for _, b := range after {
		if b.Ticker == "DHFL" {
			require.NotNil(t, b.Turnover, "turnover survives the round trip")
		}
	}
}

func TestInsertBars_CollapsesIdenticalDuplicateInSameBatch(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	bar := sampleBars()[0]
	n, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{bar, bar})
	require.NoError(t, err, "a repeated identical line in the same batch must not fail the insert")
	require.Equal(t, 1, n, "the duplicate collapses to a single version")

	after, err := store.BarsForDate(ctx, market.SourceBhavcopy, bar.Date, time.Now())
	require.NoError(t, err)
	require.Len(t, after, 1)
}

func TestInsertBars_RejectsConflictingDuplicateInSameBatch(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	bar := sampleBars()[0]
	conflicting := bar
	conflicting.Close = bar.Close + 1 // same (symbol_id, date, source), different content

	n, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{bar, conflicting})
	require.Error(t, err, "two different versions of the same (symbol, date, source) in one call must be rejected, not silently resolved")
	require.Equal(t, 0, n)
	require.Contains(t, err.Error(), bar.ISIN)

	after, err := store.BarsForDate(ctx, market.SourceBhavcopy, bar.Date, time.Now())
	require.NoError(t, err)
	require.Empty(t, after, "a rejected batch must not insert any bar for that symbol/date")
}

func TestEnsureSymbols_KeepsSymbolIDAcrossRename(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	ids, err := store.EnsureSymbols(ctx, []market.Bar{{ISIN: "INE081A01020", Ticker: "TATASTEEL"}})
	require.NoError(t, err)
	renamed, err := store.EnsureSymbols(ctx, []market.Bar{{ISIN: "INE081A01020", Ticker: "TATASTL"}})
	require.NoError(t, err)
	require.Equal(t, ids["INE081A01020"], renamed["INE081A01020"])

	var versions int
	require.NoError(t, store.Pool().QueryRow(ctx,
		"SELECT count(*) FROM symbols WHERE isin = 'INE081A01020'").Scan(&versions))
	require.Equal(t, 2, versions)
}

func TestIngestLog(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	require.NoError(t, store.LogIngest(ctx, market.SourceBhavcopy, market.Day(2015, 6, 29), "ok", 1500, ""))
	require.NoError(t, store.LogIngest(ctx, market.SourceBhavcopy, market.Day(2015, 6, 28), "no-file", 0, "404"))
	require.NoError(t, store.LogIngest(ctx, market.SourceBhavcopy, market.Day(2015, 6, 27), "error", 0, "timeout"))

	done, err := store.LoggedDates(ctx, market.SourceBhavcopy)
	require.NoError(t, err)
	require.True(t, done[market.Day(2015, 6, 29)])
	require.True(t, done[market.Day(2015, 6, 28)])
	require.False(t, done[market.Day(2015, 6, 27)], "errors are retried, so they are not done")
}

func closeOf(bars []market.Bar, ticker string) float64 {
	for _, b := range bars {
		if b.Ticker == ticker {
			return b.Close
		}
	}
	return -1
}
