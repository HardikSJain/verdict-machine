package bhavcopy

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

func countSeries(bars []market.Bar, series string) int {
	n := 0
	for _, b := range bars {
		if b.Series == series {
			n++
		}
	}
	return n
}

func find(bars []market.Bar, ticker string) (market.Bar, bool) {
	for _, b := range bars {
		if b.Ticker == ticker {
			return b, true
		}
	}
	return market.Bar{}, false
}

func TestParseLegacy_2015KeepsEQAndCarriesTurnover(t *testing.T) {
	f, err := os.Open("testdata/cm30JUN2015bhav.csv")
	require.NoError(t, err)
	defer f.Close()

	bars, err := Parse("cm30JUN2015bhav.csv", f)
	require.NoError(t, err)
	require.Len(t, bars, 1487, "1447 EQ plus 40 BE; the N2/N6/BZ rows are other instruments and stay out")
	require.Equal(t, 40, countSeries(bars, "BE"),
		"BE is the same company under trade-for-trade settlement, and dropping it punched multi-month holes in the archive")
	for _, b := range bars {
		require.True(t, Segments[b.Series], "%s came in as series %s", b.Ticker, b.Series)
	}

	dhfl, ok := find(bars, "DHFL")
	require.True(t, ok, "DHFL was listed in 2015 and is absent from eod2 today")
	require.Equal(t, "INE202B01012", dhfl.ISIN)
	require.Equal(t, market.Day(2015, 6, 30), dhfl.Date)
	require.Equal(t, 420.95, dhfl.Close)
	require.EqualValues(t, 512129, dhfl.Volume)
	require.NotNil(t, dhfl.Turnover)
	require.InDelta(t, 215046652.2, *dhfl.Turnover, 0.001)
	require.Nil(t, dhfl.DeliveryQty)
}

func TestParseLegacy_2020TwoDigitYearParses(t *testing.T) {
	f, err := os.Open("testdata/cm13JUL2020bhav.csv")
	require.NoError(t, err)
	defer f.Close()

	bars, err := Parse("cm13JUL2020bhav.csv", f)
	require.NoError(t, err)
	require.Len(t, bars, 6, "4 EQ and 2 BE; BE is kept now")

	for _, b := range bars {
		require.Equal(t, market.Day(2020, 7, 13), b.Date, "TIMESTAMP %q must parse to the two-digit-year session", "13-Jul-20")
	}

	reliance, ok := find(bars, "RELIANCE")
	require.True(t, ok)
	require.Equal(t, "INE002A01018", reliance.ISIN)
	require.Equal(t, 1903.35, reliance.Open)
	require.Equal(t, 1947.7, reliance.High)
	require.Equal(t, 1900.0, reliance.Low)
	require.Equal(t, 1935.0, reliance.Close)
	require.EqualValues(t, 32124397, reliance.Volume)
	require.NotNil(t, reliance.Turnover)
	require.InDelta(t, 61905840823.0, *reliance.Turnover, 0.01)
}

func TestParseUDiFF_2026KeepsEQAndCarriesTurnover(t *testing.T) {
	f, err := os.Open("testdata/BhavCopy_NSE_CM_0_0_0_20260828_F_0000.csv")
	require.NoError(t, err)
	defer f.Close()

	bars, err := Parse("BhavCopy_NSE_CM_0_0_0_20260828_F_0000.csv", f)
	require.NoError(t, err)
	require.Len(t, bars, 198, "180 EQ plus 18 BE")
	require.Equal(t, 18, countSeries(bars, "BE"))
	for _, b := range bars {
		require.True(t, Segments[b.Series],
			"%s came in as series %s; GB, SM and ST are other instruments", b.Ticker, b.Series)
	}

	ts, ok := find(bars, "TATASTEEL")
	require.True(t, ok)
	require.Equal(t, "INE081A01020", ts.ISIN)
	require.Equal(t, market.Day(2026, 8, 28), ts.Date)
	require.Equal(t, 186.50, ts.Close)
	require.EqualValues(t, 39370394, ts.Volume)
	require.NotNil(t, ts.Turnover)
	require.InDelta(t, 7309368090.87, *ts.Turnover, 0.01)
}

func TestURLFor_SwitchesFormatOn2024_07_08(t *testing.T) {
	require.Equal(t,
		"https://nsearchives.nseindia.com/content/historical/EQUITIES/2015/JUN/cm30JUN2015bhav.csv.zip",
		URLFor(market.Day(2015, 6, 30)))
	require.Equal(t,
		"https://nsearchives.nseindia.com/content/historical/EQUITIES/2024/JUL/cm05JUL2024bhav.csv.zip",
		URLFor(market.Day(2024, 7, 5)))
	require.Equal(t,
		"https://nsearchives.nseindia.com/content/cm/BhavCopy_NSE_CM_0_0_0_20240708_F_0000.csv.zip",
		URLFor(time.Date(2024, 7, 8, 0, 0, 0, 0, time.UTC)))
}

// TestParse_RejectsNonFiniteNumbers is the bhavcopy half of the same guard as
// eod2's TestParseDaily_RejectsNonFiniteNumbers: strconv.ParseFloat accepts
// "nan", "inf" and "-inf" with a nil error, pgx writes those into a numeric
// column, and Postgres orders NaN above every real number, so one poisoned
// price would rank first in the point-in-time universe. int64(NaN) is 0 and
// int64(+Inf) is maxint, so the quantity columns need a range check too.
func TestParse_RejectsNonFiniteNumbers(t *testing.T) {
	const header = "SYMBOL,SERIES,OPEN,HIGH,LOW,CLOSE,TOTTRDQTY,TOTTRDVAL,TIMESTAMP,ISIN\n"
	for _, tc := range []struct {
		name, row, want string
	}{
		{"nan close", "TESTCO,EQ,100.00,110.00,90.00,nan,1000,105000.00,30-Jun-2015,TESTISIN0001", "CLOSE is NaN"},
		{"inf high", "TESTCO,EQ,100.00,inf,90.00,105.00,1000,105000.00,30-Jun-2015,TESTISIN0001", "HIGH is +Inf"},
		{"-inf turnover", "TESTCO,EQ,100.00,110.00,90.00,105.00,1000,-inf,30-Jun-2015,TESTISIN0001", "TOTTRDVAL is -Inf"},
		{"nan volume", "TESTCO,EQ,100.00,110.00,90.00,105.00,nan,105000.00,30-Jun-2015,TESTISIN0001", "TOTTRDQTY is NaN"},
		{"volume past int64", "TESTCO,EQ,100.00,110.00,90.00,105.00,1e19,105000.00,30-Jun-2015,TESTISIN0001",
			"outside the range of a 64-bit integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bars, err := Parse("poisoned.csv", strings.NewReader(header+tc.row+"\n"))
			require.Error(t, err, "a non-finite or out-of-range number must not become a bar")
			require.Nil(t, bars)
			require.Contains(t, err.Error(), tc.want)
			require.Contains(t, err.Error(), "line 2")
		})
	}
}

// TestParsePreISINArchive reads NSE's archive from before it printed ISINs.
//
// The column appeared between 2011-06-01 and 2011-07-04, and this project's
// own archive starts 2011-09-02 -- not by choice but because everything here
// keys on ISIN, so the era before it could not be stored. Seventeen years of
// the source of record, including the 2000 and 2008 bear markets this project
// has never tested against, sat behind one missing column.
//
// The fixtures are real NSE files, not constructed ones. cm03NOV1994 is the
// exchange's FIRST session: 135 EQ rows, ABB and ACC among them, PREVCLOSE 0
// because there was no previous close to have.
func TestParsePreISINArchive(t *testing.T) {
	t.Run("the exchange's first session, 1994-11-03", func(t *testing.T) {
		f, err := os.Open("testdata/cm03NOV1994bhav.csv")
		require.NoError(t, err)
		defer f.Close()

		bars, err := Parse("cm03NOV1994bhav.csv", f)
		require.NoError(t, err)
		require.Len(t, bars, 135, "every row in the first session is series EQ")

		first := bars[0]
		require.Equal(t, "ABB", first.Ticker)
		require.Equal(t, "EQ", first.Series)
		// The day is NOT zero-padded in the older files ("3-NOV-1994"), which
		// the four-digit padded format string rejects outright.
		require.Equal(t, market.Day(1994, 11, 3), first.Date)
		require.Equal(t, 715.0, first.Close)
		require.Equal(t, int64(100), first.Volume)
		require.NotNil(t, first.Turnover)
		require.Equal(t, 71500.0, *first.Turnover)

		// The truthful reading of a file with no ISIN column is no ISIN, and
		// every bar has to say so. Inventing one here is what would make a
		// later identity decision unauditable.
		for _, b := range bars {
			require.Empty(t, b.ISIN, "%s must carry no ISIN, not a fabricated one", b.Ticker)
		}
	})

	t.Run("2007, mid-era, with non-EQ series to filter", func(t *testing.T) {
		f, err := os.Open("testdata/cm01OCT2007bhav.csv")
		require.NoError(t, err)
		defer f.Close()

		bars, err := Parse("cm01OCT2007bhav.csv", f)
		require.NoError(t, err)
		require.Len(t, bars, 39, "36 EQ and 3 BE")
		require.Equal(t, "3IINFOTECH", bars[0].Ticker)
		require.Equal(t, market.Day(2007, 10, 1), bars[0].Date)
		require.InDelta(t, 150.25, bars[0].Close, 1e-9)
		for _, b := range bars {
			require.True(t, Segments[b.Series])
			require.Empty(t, b.ISIN)
		}
	})
}

// TestParseStillPrefersTheISINLayoutWhereBothCouldMatch pins the dispatch
// order. The pre-ISIN header is a strict SUBSET of the legacy one -- SYMBOL,
// SERIES and TIMESTAMP are in both -- so a switch that tested the shorter
// header first would read every modern file as identity-less and throw away
// fifteen years of ISINs without erroring once.
func TestParseStillPrefersTheISINLayoutWhereBothCouldMatch(t *testing.T) {
	f, err := os.Open("testdata/cm30JUN2015bhav.csv")
	require.NoError(t, err)
	defer f.Close()

	bars, err := Parse("cm30JUN2015bhav.csv", f)
	require.NoError(t, err)
	require.NotEmpty(t, bars)
	for _, b := range bars {
		require.NotEmpty(t, b.ISIN, "%s lost its ISIN to the pre-ISIN layout", b.Ticker)
	}
}

// TestArchiveStartIsTheDayTheExchangeOpened records a measured constant.
//
// Every other loader here carries one; bhavcopy was the only one without, so
// a backfill from an optimistic date would have probed roughly 1,800 dead
// dates before reaching real data. Probed 2026-09-10: 1994-11-03 serves a zip
// with 135 EQ rows and every date before it 404s.
func TestArchiveStartIsTheDayTheExchangeOpened(t *testing.T) {
	require.Equal(t, market.Day(1994, 11, 3), ArchiveStart)

	f, err := os.Open("testdata/cm03NOV1994bhav.csv")
	require.NoError(t, err)
	defer f.Close()
	bars, err := Parse("first", f)
	require.NoError(t, err)
	require.Equal(t, ArchiveStart, bars[0].Date,
		"the constant and the exchange's first session must be the same day")
}

// TestBlockDealsWouldShadowTheRealSessionAndAreRefused is why Segments is an
// allowlist rather than a blocklist.
//
// bars is keyed on (symbol_id, date, source, ingested_at). SERIES IS NOT IN
// THE KEY. So two rows for one security on one session are indistinguishable
// from a corrected bar, and the store keeps the later one -- a block deal,
// which is a single negotiated trade, would silently replace that session's
// real open, high, low and close, and no reader downstream could tell.
//
// It is not hypothetical: SFCL on 2015-06-30 and FAIRCHEM on 2018-10-01 each
// appear under BL and EQ with the same ISIN on the same date in NSE's own
// files. EQ and BE never collide that way, which is exactly why those two are
// the list.
//
// The guard lives in the parser so widening Segments fails loudly at the point
// of entry instead of quietly rewriting a price.
func TestBlockDealsWouldShadowTheRealSessionAndAreRefused(t *testing.T) {
	const csv = "SYMBOL,SERIES,OPEN,HIGH,LOW,CLOSE,LAST,PREVCLOSE,TOTTRDQTY,TOTTRDVAL,TIMESTAMP,TOTALTRADES,ISIN\n" +
		"SFCL,EQ,10,11,9,10.5,10.5,10,1000,10500,30-JUN-2015,50,INE935O01010\n" +
		"SFCL,BL,10.4,10.4,10.4,10.4,10.4,10,500000,5200000,30-JUN-2015,1,INE935O01010\n"

	// As shipped, BL is not in Segments, so only the real session survives.
	bars, err := Parse("collision", strings.NewReader(csv))
	require.NoError(t, err)
	require.Len(t, bars, 1)
	require.Equal(t, "EQ", bars[0].Series)
	require.Equal(t, 10.5, bars[0].Close, "the real close, not the block deal's")

	// Widen the allowlist and the guard must stop it rather than let one row
	// overwrite the other.
	Segments["BL"] = true
	defer delete(Segments, "BL")

	_, err = Parse("collision", strings.NewReader(csv))
	require.Error(t, err, "a security appearing twice in one session must be refused, not stored twice")
	require.ErrorContains(t, err, "SFCL")
	require.ErrorContains(t, err, "2015-06-30")
	require.ErrorContains(t, err, "revision")
}
