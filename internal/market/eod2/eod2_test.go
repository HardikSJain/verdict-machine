package eod2

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

const eod2Header = "Date,Open,High,Low,Close,Volume,Series,TOTAL_TRADES,QTY_PER_TRADE,DLV_QTY"

func TestParseDaily_TataSteelSplitDayIsContinuous(t *testing.T) {
	f, err := os.Open("testdata/daily/tatasteel.csv")
	require.NoError(t, err)
	defer f.Close()

	bars, err := ParseDaily(f, "TATASTEEL", "INE081A01020")
	require.NoError(t, err)
	require.Len(t, bars, 13)

	byDate := map[string]market.Bar{}
	for _, b := range bars {
		byDate[b.Date.Format("2006-01-02")] = b
	}
	before := byDate["2022-07-27"]
	on := byDate["2022-07-28"]
	require.Equal(t, "INE081A01020", on.ISIN)
	require.Equal(t, "EQ", on.Series)
	require.Nil(t, on.Turnover, "eod2 does not report rupee turnover")
	require.NotNil(t, on.DeliveryQty)
	require.EqualValues(t, 52403544, *on.DeliveryQty)

	// Exact OHLCV for the 2022-07-28 fixture row, so a column-mapping bug (e.g.
	// reading High into Close) fails here even when it would still satisfy the
	// loose split-continuity ratio below.
	require.Equal(t, 98.1, on.Open)
	require.Equal(t, 102.0, on.High)
	require.Equal(t, 97.15, on.Low)
	require.Equal(t, 100.35, on.Close)
	require.EqualValues(t, 137156107, on.Volume)

	// Tata Steel split 1:10 on 2022-07-28. eod2 adjusts history in place, so the
	// series must be continuous across the ex-date; unadjusted data would show ~0.1.
	ratio := on.Close / before.Close
	require.Greater(t, ratio, 0.8)
	require.Less(t, ratio, 1.25)
}

func TestParseDaily_EmptyReaderReturnsError(t *testing.T) {
	// No header line at all: cr.Read() hits EOF immediately.
	_, err := ParseDaily(strings.NewReader(""), "TATASTEEL", "INE081A01020")
	require.Error(t, err)
}

func TestParseDaily_MissingRequiredColumnReturnsError(t *testing.T) {
	// Header omits Close.
	csv := "Date,Open,High,Low,Volume,Series\n" +
		"2022-07-28,98.1,102.0,97.15,137156107,EQ\n"
	_, err := ParseDaily(strings.NewReader(csv), "TATASTEEL", "INE081A01020")
	require.Error(t, err)
	require.Contains(t, err.Error(), "Close")
}

func TestParseDaily_BadDateReturnsError(t *testing.T) {
	csv := eod2Header + "\n" +
		"28-07-2022,98.1,102.0,97.15,100.35,137156107,EQ,628262.0,218.31,52403544.0\n"
	_, err := ParseDaily(strings.NewReader(csv), "TATASTEEL", "INE081A01020")
	require.Error(t, err)
}

func TestParseDaily_NonNumericFieldReturnsError(t *testing.T) {
	csv := eod2Header + "\n" +
		"2022-07-28,not-a-number,102.0,97.15,100.35,137156107,EQ,628262.0,218.31,52403544.0\n"
	_, err := ParseDaily(strings.NewReader(csv), "TATASTEEL", "INE081A01020")
	require.Error(t, err)
}

// TestParseDaily_ShortRowReturnsError feeds a data row with fewer fields than
// the required columns need (Date,Open,High,Low,Close only — no Volume or
// Series). Before the len(rec) < minFields guard in ParseDaily, this panicked
// with an index-out-of-range instead of returning an error.
func TestParseDaily_ShortRowReturnsError(t *testing.T) {
	csv := eod2Header + "\n" +
		"2022-07-28,98.1,102.0,97.15,100.35\n"
	bars, err := ParseDaily(strings.NewReader(csv), "TATASTEEL", "INE081A01020")
	require.Error(t, err)
	require.Nil(t, bars)
	require.Contains(t, err.Error(), "line 2")
}

func TestLoadSymbolMap(t *testing.T) {
	m, err := LoadSymbolMap("testdata/isin_symbol_map.json")
	require.NoError(t, err)
	require.Equal(t, "INE081A01020", m["TATASTEEL"])
}

func TestLoadDir_SkipsTickersWithoutISIN(t *testing.T) {
	// testdata/daily also has reliance.CSV (see
	// TestLoadDir_MatchesUpperCaseCSVExtension below), so mapping only
	// RELIANCE loads that file and skips tatasteel.csv, and vice versa.
	bars, skipped, err := LoadDir("testdata/daily", map[string]string{"RELIANCE": "INE002A01018"})
	require.NoError(t, err)
	require.Len(t, bars, 2)
	require.Equal(t, []string{"TATASTEEL"}, skipped)

	bars, skipped, err = LoadDir("testdata/daily", map[string]string{"TATASTEEL": "INE081A01020"})
	require.NoError(t, err)
	require.Len(t, bars, 13)
	require.Equal(t, []string{"RELIANCE"}, skipped)
}

// TestLoadDir_MatchesUpperCaseCSVExtension proves LoadDir treats the .csv
// extension case-insensitively. Before the fix, the filter
// strings.HasSuffix(e.Name(), ".csv") was case-sensitive, so a file named
// with an upper-case ".CSV" extension (as some data providers produce) was
// silently dropped: not loaded into bars, and not reported in skipped
// either, since the extension filter excluded it before the ticker was ever
// looked up against sym2isin.
func TestLoadDir_MatchesUpperCaseCSVExtension(t *testing.T) {
	bars, skipped, err := LoadDir("testdata/daily", map[string]string{"RELIANCE": "INE002A01018"})
	require.NoError(t, err)
	require.NotContains(t, skipped, "RELIANCE")

	var got []market.Bar
	for _, b := range bars {
		if b.Ticker == "RELIANCE" {
			got = append(got, b)
		}
	}
	require.Len(t, got, 2, "reliance.CSV should have been loaded despite its upper-case extension")
	require.Equal(t, "INE002A01018", got[0].ISIN)
}
