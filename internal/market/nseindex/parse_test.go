package nseindex_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/nseindex"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

const header = "Index Name,Index Date,Open Index Value,High Index Value,Low Index Value," +
	"Closing Index Value,Points Change,Change(%),Volume,Turnover (Rs. Cr.),P/E,P/B,Div Yield\n"

// Rows copied verbatim from the real archive, one per era, so the parser is
// tested against NSE's own bytes rather than against a tidied-up imitation.
const (
	// nsearchives.nseindia.com/content/indices/ind_close_all_02042012.csv
	rowSPCNX = "S&P CNX Nifty,02-04-2012,5296.35,5331.55,5278.8,5317.9,22.35,.42,134538287,4583.68,18.79,3.02,1.5\n"
	// ..._02012014.csv
	rowCNX = "CNX Nifty,02-01-2014,6301.25,6358.3,6211.3,6221.15,-80.5,-1.28,158132556,5249.79,18.46,2.95,1.5\n"
	// ..._04092026.csv
	rowNifty50 = "Nifty 50,04-09-2026,23910.9,24005.75,23895.85,23897.7,24.25,.1,231396362,18379.67,20.2,2.89,1.19\n"
	// ..._04092026.csv -- an index computed once a day: no open, high, low,
	// volume, turnover or ratios.
	rowDashes = "Nifty 50 Futures Index,04-09-2026,-,-,-,5877.06,11.73,.2,-,-,-,-,-\n"
)

func TestParseRealRowsAcrossEveryEra(t *testing.T) {
	for _, tc := range []struct {
		name     string
		row      string
		date     time.Time
		wantCode string
		wantName string
		close    float64
	}{
		{"2012 S&P CNX Nifty", rowSPCNX, day(2012, 4, 2), "NIFTY50", "S&P CNX Nifty", 5317.9},
		{"2014 CNX Nifty", rowCNX, day(2014, 1, 2), "NIFTY50", "CNX Nifty", 6221.15},
		{"2026 Nifty 50", rowNifty50, day(2026, 9, 4), "NIFTY50", "Nifty 50", 23897.7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nseindex.Parse(strings.NewReader(header+tc.row), tc.date)
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, tc.wantCode, got[0].IndexCode,
				"three published names, one index: keying on the printed name would split fourteen years into three series")
			require.Equal(t, tc.wantName, got[0].IndexName,
				"and the name as printed is kept, so the canonical choice stays auditable")
			require.Equal(t, tc.close, got[0].Close)
			require.NotNil(t, got[0].Open)
			require.NotNil(t, got[0].DivYield)
		})
	}
}

// TestDashIsNullNotZero. 27 of the 165 indices in a recent file publish only a
// close. A zero open would give them a daily range equal to their entire level
// and put them bottom of any ranking that touched it.
func TestDashIsNullNotZero(t *testing.T) {
	got, err := nseindex.Parse(strings.NewReader(header+rowDashes), day(2026, 9, 4))
	require.NoError(t, err)
	require.Len(t, got, 1)
	l := got[0]
	require.Equal(t, 5877.06, l.Close)
	require.Nil(t, l.Open)
	require.Nil(t, l.High)
	require.Nil(t, l.Low)
	require.Nil(t, l.Volume)
	require.Nil(t, l.Turnover)
	require.Nil(t, l.PE)
	require.Nil(t, l.DivYield)
	require.NotNil(t, l.PointsChange, "points change is published even when the rest is not")
}

// TestLeadingDotFloats: the archive writes small percentages as ".1" and
// "-.23".
func TestLeadingDotFloats(t *testing.T) {
	got, err := nseindex.Parse(strings.NewReader(header+rowNifty50), day(2026, 9, 4))
	require.NoError(t, err)
	require.NotNil(t, got[0].PctChange)
	require.InDelta(t, 0.1, *got[0].PctChange, 1e-9)
}

// TestParseRefusesAFileForTheWrongSession. The archive has always agreed with
// its own filename; this is what keeps that a fact rather than an assumption.
// NSE serving yesterday's file under today's URL would otherwise write stale
// levels under today's date and stay invisible until a trend rule acted on it.
func TestParseRefusesAFileForTheWrongSession(t *testing.T) {
	_, err := nseindex.Parse(strings.NewReader(header+rowNifty50), day(2026, 9, 5))
	require.Error(t, err)
	require.Contains(t, err.Error(), "served the wrong session")
}

// TestParseRefusesAChangedHeader. A silent column reorder would file turnover
// where volume belongs and nothing downstream would ever notice.
func TestParseRefusesAChangedHeader(t *testing.T) {
	swapped := strings.Replace(header,
		"Volume,Turnover (Rs. Cr.)", "Turnover (Rs. Cr.),Volume", 1)
	_, err := nseindex.Parse(strings.NewReader(swapped+rowNifty50), day(2026, 9, 4))
	require.Error(t, err)
	require.Contains(t, err.Error(), "the archive's layout changed")
}

// TestParseRefusesADuplicateIndex: two rows for one index on one session would
// silently overwrite each other under a (code, date) key.
func TestParseRefusesADuplicateIndex(t *testing.T) {
	_, err := nseindex.Parse(strings.NewReader(header+rowNifty50+rowNifty50), day(2026, 9, 4))
	require.Error(t, err)
	require.Contains(t, err.Error(), "appears twice")
}

// TestParseRefusesAnEmptyFile rather than reporting a session with no indices,
// which would settle in ingest_log as a successful ingest of nothing.
func TestParseRefusesAnEmptyFile(t *testing.T) {
	_, err := nseindex.Parse(strings.NewReader(header), day(2026, 9, 4))
	require.Error(t, err)
	require.Contains(t, err.Error(), "no rows")
}

// TestUncuratedNamesStayApart is the safe direction of the identity rule.
//
// Two series that should be one is a visible gap somebody can close; one series
// that should be two is a silent lie nothing can detect afterwards. So an
// uncurated rename produces two codes, not a guess.
func TestUncuratedNamesStayApart(t *testing.T) {
	require.Equal(t, "CNXSMALLCAP", nseindex.CodeFor("CNX Smallcap", day(2012, 4, 2)))
	require.Equal(t, "NIFTYSMALLCAP100", nseindex.CodeFor("Nifty Smallcap 100", day(2026, 9, 4)))
	require.NotEqual(t,
		nseindex.CodeFor("CNX Smallcap", day(2012, 4, 2)),
		nseindex.CodeFor("Nifty Smallcap 100", day(2026, 9, 4)))
}

// TestCuratedAliasesCollapseToOneCode, and the curated set is small on purpose.
func TestCuratedAliasesCollapseToOneCode(t *testing.T) {
	for _, name := range []string{"S&P CNX Nifty", "CNX Nifty", "Nifty 50", "  nifty 50  "} {
		require.Equal(t, nseindex.Nifty50, nseindex.CodeFor(name, day(2020, 1, 1)),
			"%q must resolve to the canonical Nifty 50", name)
	}
	names, err := nseindex.NamesFor(nseindex.Nifty50)
	require.NoError(t, err)
	require.Len(t, names, 3)

	_, err = nseindex.NamesFor("NOT_A_CODE")
	require.Error(t, err)
	require.NotEmpty(t, nseindex.CuratedCodes())
}

// TestContentHashChangesWithAnyPublishedValue, so a re-ingest that changes
// nothing is a no-op and a correction is a new version.
func TestContentHashChangesWithAnyPublishedValue(t *testing.T) {
	base, err := nseindex.Parse(strings.NewReader(header+rowNifty50), day(2026, 9, 4))
	require.NoError(t, err)
	same, err := nseindex.Parse(strings.NewReader(header+rowNifty50), day(2026, 9, 4))
	require.NoError(t, err)
	require.Equal(t, base[0].ContentHash(), same[0].ContentHash())

	corrected := strings.Replace(rowNifty50, "23897.7", "23897.8", 1)
	other, err := nseindex.Parse(strings.NewReader(header+corrected), day(2026, 9, 4))
	require.NoError(t, err)
	require.NotEqual(t, base[0].ContentHash(), other[0].ContentHash())

	// A null and a zero must not hash alike, or a "-" turning into a real zero
	// would look like no change at all.
	var zero float64
	a := market.IndexLevel{IndexCode: "X", Date: day(2026, 9, 4), Close: 1}
	b := a
	b.Open = &zero
	require.NotEqual(t, a.ContentHash(), b.ContentHash())
}
