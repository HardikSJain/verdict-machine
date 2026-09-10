package market_test

import (
	"context"
	"fmt"
	"sync"
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

// TestEnsureSymbols_RejectsBarWithNoDate covers the one bad input that would
// otherwise be written rather than rejected. valid_from 0001-01-01 is not a
// date the store can hold; it is the sentinel migration 0002 stamped on the
// rows written before valid_from existed, and it means "applies to every bar
// date". A zero time.Time out of a broken parser lands exactly there, and
// symbols is insert-only, so the two would be indistinguishable forever with
// no UPDATE available to separate them.
func TestEnsureSymbols_RejectsBarWithNoDate(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	_, err := store.EnsureSymbols(ctx, []market.Bar{{ISIN: "INE081A01020", Ticker: "TATASTEEL"}})
	require.ErrorContains(t, err, "TATASTEEL")
	require.ErrorContains(t, err, "has no date")

	var n int
	require.NoError(t, store.Pool().QueryRow(ctx, "SELECT count(*) FROM symbols").Scan(&n))
	require.Zero(t, n, "the batch is rejected before anything is written")

	// The same batch with a date is accepted, so the check rejects the missing
	// date and not the bar.
	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE081A01020", Ticker: "TATASTEEL", Date: market.Day(2015, 6, 30)}})
	require.NoError(t, err)
	require.Len(t, ids, 1)
}

func TestEnsureSymbols_KeepsSymbolIDAcrossRename(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	// Both observations carry the session date they were seen for. They used
	// to carry none, which meant both landed on valid_from 0001-01-01 -- the
	// pre-migration sentinel -- and the rename this test is about was recorded
	// as having happened before the calendar starts.
	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE081A01020", Ticker: "TATASTEEL", Date: market.Day(2015, 6, 30)}})
	require.NoError(t, err)
	renamed, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: "INE081A01020", Ticker: "TATASTL", Date: market.Day(2016, 6, 30)}})
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

func closeOf(bars []market.StoredBar, ticker string) float64 {
	for _, b := range bars {
		if b.Ticker == ticker {
			return b.Close
		}
	}
	return -1
}

// barAt is a minimal, self-consistent EQ bar: enough to be inserted, ranked
// and labelled, with the ticker and the session date being the parts under test.
func barAt(isin, ticker string, d time.Time, close float64) market.Bar {
	turnover := close * 1000
	return market.Bar{ISIN: isin, Ticker: ticker, Series: "EQ", Date: d,
		Open: close, High: close, Low: close, Close: close, Volume: 1000, Turnover: &turnover}
}

// TestBarsForDate_TickerIsTheOneInForceOnThatSession pins the as-of-calendar
// axis. Before symbols carried valid_from, both readers resolved the label
// with `ORDER BY ingested_at DESC LIMIT 1`, so every bar of every date was
// labelled with whichever version happened to land last -- and interleaving
// `ingest eod2` (which maps a ticker's whole history to today's name) with a
// `backfill` still walking 2015 made that oscillate. A point-in-time store
// that cannot say what a symbol was called on a date is not point-in-time.
func TestBarsForDate_TickerIsTheOneInForceOnThatSession(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	const isin = "INE095I01015"
	old2015 := market.Day(2015, 6, 30)
	new2016 := market.Day(2016, 6, 30)

	_, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{barAt(isin, "SONASTEER", old2015, 100)})
	require.NoError(t, err)
	_, err = store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{barAt(isin, "JTEKTINDIA", new2016, 120)})
	require.NoError(t, err)

	label := func(d time.Time) string {
		bars, err := store.BarsForDate(ctx, market.SourceBhavcopy, d, time.Now())
		require.NoError(t, err)
		require.Len(t, bars, 1)
		return bars[0].Ticker
	}
	require.Equal(t, "SONASTEER", label(old2015), "a 2015 bar carries the 2015 ticker, not today's")
	require.Equal(t, "JTEKTINDIA", label(new2016))

	versions := func() int {
		var n int
		require.NoError(t, store.Pool().QueryRow(ctx,
			"SELECT count(*) FROM symbols WHERE isin = $1", isin).Scan(&n))
		return n
	}
	require.Equal(t, 2, versions(), "one rename, two versions")

	// Replaying the two sessions in either order -- what two ingest processes
	// running side by side amount to -- must write no further versions and
	// change no label.
	for i := 0; i < 3; i++ {
		_, err = store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{barAt(isin, "SONASTEER", old2015, 100)})
		require.NoError(t, err)
		_, err = store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{barAt(isin, "JTEKTINDIA", new2016, 120)})
		require.NoError(t, err)
	}
	require.Equal(t, 2, versions(), "replaying an old session must not re-assert the old name over the newer one")
	require.Equal(t, "SONASTEER", label(old2015))
	require.Equal(t, "JTEKTINDIA", label(new2016))
}

// TestBarsForDate_TickerIsAsOfIngestTimestamp pins the other axis: the
// `symbols.ingested_at <= asOfIngest` predicate in the reader's lateral join.
// Replacing it with `(ingested_at <= $3 OR true)` left the whole suite green
// before this test existed: the write side's rename behaviour was covered,
// the read side's was not.
func TestBarsForDate_TickerIsAsOfIngestTimestamp(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	const isin = "INE095I01015"
	d := market.Day(2015, 6, 30)

	_, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{barAt(isin, "OLD", d, 100)})
	require.NoError(t, err)
	firstIngest := time.Now()
	time.Sleep(20 * time.Millisecond)

	// A corrected file for that same session, carrying a different ticker.
	_, err = store.EnsureSymbols(ctx, []market.Bar{{ISIN: isin, Ticker: "NEW", Date: d}})
	require.NoError(t, err)

	before, err := store.BarsForDate(ctx, market.SourceBhavcopy, d, firstIngest)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Equal(t, "OLD", before[0].Ticker, "as-of an earlier ingest, the label is the one the store held then")

	after, err := store.BarsForDate(ctx, market.SourceBhavcopy, d, time.Now())
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Equal(t, "NEW", after[0].Ticker)
}

// TestEnsureSymbols_ConcurrentRegistrationKeepsOneSymbolID reproduces the
// race the old non-transactional loop had: two callers both read "this ISIN
// is unknown" and both mint, and every constraint on the table passes because
// their autocommit transactions get different now() values. Since every
// market table is insert-only there is no UPDATE or DELETE to repair it -- the
// company would be two companies forever. `verdict backfill` and
// `verdict ingest eod2` are separate processes the README puts side by side,
// and a 15-year backfill runs for hours, so the window is wide, not narrow.
func TestEnsureSymbols_ConcurrentRegistrationKeepsOneSymbolID(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	// Several rounds of a fresh ISIN, because the window is a scheduling
	// accident: one round can happen to serialize on its own.
	const (
		callers = 4
		rounds  = 25
	)
	for round := 0; round < rounds; round++ {
		isin := fmt.Sprintf("INE111Z%05d", round)
		bars := []market.Bar{{ISIN: isin, Ticker: "NEWCO", Date: market.Day(2024, 1, 2)}}

		ids := make([]int64, callers)
		errs := make([]error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				got, err := store.EnsureSymbols(ctx, bars)
				errs[i] = err
				if err == nil {
					ids[i] = got[isin]
				}
			}(i)
		}
		close(start)
		wg.Wait()

		for i := range errs {
			require.NoError(t, errs[i], "round %d, caller %d", round, i)
			require.Equal(t, ids[0], ids[i], "round %d: every caller must be told the same symbol_id", round)
		}
		var distinct int
		require.NoError(t, store.Pool().QueryRow(ctx,
			"SELECT count(DISTINCT symbol_id) FROM symbols WHERE isin = $1", isin).Scan(&distinct))
		require.Equal(t, 1, distinct,
			"round %d: one ISIN owns exactly one symbol_id, whatever the concurrency", round)
	}
}

// TestInsertBars_RevisesANonCloseField exercises the versioning path end to
// end on a field no other test touches. The existing coverage revises only
// Close (store_test.go) and Turnover (universe_test.go), so a field silently
// dropped from ContentHash would make InsertBars do the opposite of what it
// promises: keep the stale row and discard the correction, permanently, with
// no error. eod2 rewrites its daily CSVs in place and its bars never carry
// turnover, so a re-issued file whose only change is a corrected DLV_QTY is
// exactly the case that would have been swallowed.
func TestInsertBars_RevisesANonCloseField(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d := market.Day(2022, 7, 28)
	qty := int64(52403544)
	bar := market.Bar{ISIN: "INE081A01020", Ticker: "TATASTEEL", Series: "EQ", Date: d,
		Open: 98.1, High: 102, Low: 97.15, Close: 100.35, Volume: 137156107, DeliveryQty: &qty}

	n, err := store.InsertBars(ctx, market.SourceEod2, []market.Bar{bar})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	corrected := bar
	correctedQty := qty + 1000
	corrected.DeliveryQty = &correctedQty
	n, err = store.InsertBars(ctx, market.SourceEod2, []market.Bar{corrected})
	require.NoError(t, err)
	require.Equal(t, 1, n, "a corrected delivery quantity is a new version, not an unchanged bar")

	after, err := store.BarsForDate(ctx, market.SourceEod2, d, time.Now())
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.NotNil(t, after[0].DeliveryQty)
	require.EqualValues(t, correctedQty, *after[0].DeliveryQty)
}

// TestEnsureSymbolsRefusesPreISINBars is the guard that keeps a parser change
// from becoming a data disaster.
//
// NSE printed no ISIN column before July 2011, so bars parsed from that era
// arrive with none -- which is the file telling the truth, not a fault. The
// danger is what happens next. isin is the identity key: symbols is unique on
// it, and every entity, roster link and universe read resolves through it. The
// column is NOT NULL, and "" is not NULL, so storing these would not fail. It
// would quietly file EVERY pre-ISIN ticker of EVERY session under one symbol
// row -- a single company with thousands of contradictory prices per day --
// and nothing downstream would notice, because nothing downstream checks.
//
// So the refusal is here, at the one place identity is assigned, and it names
// the era rather than only the symptom.
func TestEnsureSymbolsRefusesPreISINBars(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	_, err := store.EnsureSymbols(ctx, []market.Bar{
		{Ticker: "ABB", Series: "EQ", Date: market.Day(1994, 11, 3),
			Open: 715, High: 715, Low: 715, Close: 715, Volume: 100},
	})
	require.Error(t, err, "a bar with no identity must be refused, not filed under the empty string")
	require.ErrorContains(t, err, "ABB")
	require.ErrorContains(t, err, "1994-11-03")
	require.ErrorContains(t, err, "before July 2011",
		"the error must name the era, so whoever hits it knows this is the identity problem and not a corrupt file")
}
