package bhavcopy

import (
	"os"
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
