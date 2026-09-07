package eod2

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

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

	// Tata Steel split 1:10 on 2022-07-28. eod2 adjusts history in place, so the
	// series must be continuous across the ex-date; unadjusted data would show ~0.1.
	ratio := on.Close / before.Close
	require.Greater(t, ratio, 0.8)
	require.Less(t, ratio, 1.25)
}

func TestLoadSymbolMap(t *testing.T) {
	m, err := LoadSymbolMap("testdata/isin_symbol_map.json")
	require.NoError(t, err)
	require.Equal(t, "INE081A01020", m["TATASTEEL"])
}

func TestLoadDir_SkipsTickersWithoutISIN(t *testing.T) {
	bars, skipped, err := LoadDir("testdata/daily", map[string]string{"RELIANCE": "INE002A01018"})
	require.NoError(t, err)
	require.Empty(t, bars)
	require.Equal(t, []string{"TATASTEEL"}, skipped)

	bars, skipped, err = LoadDir("testdata/daily", map[string]string{"TATASTEEL": "INE081A01020"})
	require.NoError(t, err)
	require.Len(t, bars, 13)
	require.Empty(t, skipped)
}
