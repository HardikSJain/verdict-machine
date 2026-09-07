package market

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// TestSymbolLabelFallbackAgreesWriterAndReader pins the one question the Go
// writer (versionAt) and the SQL reader (symbolLabelLateral) each have to
// answer by invention rather than by lookup: what is a symbol called on a bar
// date that predates every recorded version of it? eod2 makes this the common
// case, not a corner one -- it registers a symbol at today's date and then
// reports fifteen years of history under it, so most of that history predates
// the only version there is.
//
// The two sides resolve that fallback in different languages, in different
// files, and nothing forced them to agree; disagreeing means one bar resolves
// to two different tickers depending on which side of the store you ask, and
// the write side's "has this been renamed?" test then fires on a name the read
// side never returns. This test is deliberately in package market so it can
// ask both sides the same question and compare the two answers directly.
func TestSymbolLabelFallbackAgreesWriterAndReader(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Pool(t)
	s := NewStore(pool)

	const isin = "INE095I01015"
	v2015 := Day(2015, 6, 30)
	v2016 := Day(2016, 6, 30)
	before := Day(2014, 1, 2) // predates every version below

	ids, err := s.EnsureSymbols(ctx, []Bar{{ISIN: isin, Ticker: "SONASTEER", Date: v2015}})
	require.NoError(t, err)
	id := ids[isin]
	_, err = s.EnsureSymbols(ctx, []Bar{{ISIN: isin, Ticker: "JTEKTINDIA", Date: v2016}})
	require.NoError(t, err)
	// A correction to what that 2015 session called the symbol, written later.
	// It ties the earliest version on valid_from and breaks it on ingested_at,
	// which is the tiebreak the two sides used to resolve in opposite
	// directions: the writer read its version list ingested_at ascending and
	// took the first, the reader ordered ingested_at DESC and took the last.
	time.Sleep(10 * time.Millisecond)
	_, err = s.EnsureSymbols(ctx, []Bar{{ISIN: isin, Ticker: "SONACOMS", Date: v2015}})
	require.NoError(t, err)

	var versions []symbolVersion
	rows, err := pool.Query(ctx,
		`SELECT symbol_id, ticker, valid_from FROM symbols WHERE isin = $1 ORDER BY valid_from, ingested_at`, isin)
	require.NoError(t, err)
	for rows.Next() {
		var v symbolVersion
		var from time.Time
		require.NoError(t, rows.Scan(&v.id, &v.ticker, &from))
		v.validFrom = Day(from.Year(), from.Month(), from.Day())
		versions = append(versions, v)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.Len(t, versions, 3, "two names plus one same-session correction")

	writer := versionAt(versions, before).ticker

	// The reader's answer for the same date. The bar is inserted with raw SQL
	// rather than InsertBars so that registering it cannot itself write the
	// version whose absence is the whole point of the test.
	_, err = pool.Exec(ctx,
		`INSERT INTO bars (symbol_id, date, source, series, open, high, low, close, volume, content_hash)
		 VALUES ($1, $2, $3, 'EQ', 90, 90, 90, 90, 1000, $4)`,
		id, before, SourceBhavcopy, []byte{1})
	require.NoError(t, err)
	bars, err := s.BarsForDate(ctx, SourceBhavcopy, before, time.Now())
	require.NoError(t, err)
	require.Len(t, bars, 1)
	reader := bars[0].Ticker

	require.Equal(t, writer, reader,
		"the Go writer and the SQL reader must resolve a pre-history bar to the same ticker")
	require.Equal(t, "SONACOMS", reader,
		"the fallback is the earliest recorded valid_from, and the latest correction to it")
}
