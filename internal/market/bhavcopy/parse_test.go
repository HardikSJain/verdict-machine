package bhavcopy

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

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
	require.Len(t, bars, 1447, "only series EQ rows are kept")

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
	require.Len(t, bars, 4, "only series EQ rows are kept; the two BE rows are filtered out")

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
	require.NotEmpty(t, bars)
	for _, b := range bars {
		require.Equal(t, "EQ", b.Series)
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
