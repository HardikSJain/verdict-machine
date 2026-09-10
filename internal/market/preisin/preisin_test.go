package preisin_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/preisin"
)

// sessions is a fake archive calendar: gaps are counted in SESSIONS, so the
// tests have to supply one rather than subtract dates.
func sessionGap(cal []time.Time) func(a, b time.Time) int {
	idx := map[time.Time]int{}
	for i, d := range cal {
		idx[d] = i
	}
	return func(a, b time.Time) int {
		ia, oka := idx[a]
		ib, okb := idx[b]
		if !oka || !okb {
			return 1 << 30 // unknown means "too far", never "close enough"
		}
		return ib - ia
	}
}

func cal(days ...time.Time) []time.Time { return days }

func TestResolveBridgesOnlyAcrossADirectBoundary(t *testing.T) {
	c := cal(
		market.Day(2011, 7, 1), market.Day(2011, 7, 4), market.Day(2011, 7, 5),
		market.Day(2011, 7, 6), market.Day(2011, 7, 7), market.Day(2011, 7, 8),
		market.Day(2011, 7, 11), market.Day(2011, 7, 12),
	)
	gap := sessionGap(c)

	got, sum := preisin.Resolve([]preisin.Observation{
		// Traded the last pre-ISIN session and the first ISIN one: same
		// instrument, no question.
		{Ticker: "ABB", LastPreISIN: market.Day(2011, 7, 1),
			FirstISINEra: market.Day(2011, 7, 4), ISINEraISIN: "INE117A01022"},
		// Six sessions apart -- past the measured threshold, so not merged.
		{Ticker: "REUSED", LastPreISIN: market.Day(2011, 7, 1),
			FirstISINEra: market.Day(2011, 7, 12), ISINEraISIN: "INE999Z01011"},
		// Never came back: a company that died before ISINs existed.
		{Ticker: "GONE", LastPreISIN: market.Day(2011, 7, 1)},
	}, gap, preisin.MaxBridgeGapSessions)

	require.Equal(t, preisin.Summary{Bridged: 1, Dead: 1, Quarantined: 1}, sum)
	require.Equal(t, 3, sum.Total())

	byTicker := map[string]preisin.Assignment{}
	for _, a := range got {
		byTicker[a.Ticker] = a
	}

	abb := byTicker["ABB"]
	require.Equal(t, preisin.Bridged, abb.Status)
	require.Equal(t, "INE117A01022", abb.ISIN, "a bridged ticker takes NSE's own ISIN")
	require.False(t, abb.Synthetic)
	require.Equal(t, 1, abb.GapSessions)

	// The one that matters. Welding two companies into one price series is
	// invisible once done and poisons every backtest that touches it, so the
	// resolver must refuse rather than pick.
	reused := byTicker["REUSED"]
	require.Equal(t, preisin.Quarantined, reused.Status)
	require.True(t, reused.Synthetic)
	require.Equal(t, "X-NSE-REUSED", reused.ISIN)
	require.NotEqual(t, "INE999Z01011", reused.ISIN,
		"a wide gap must never silently adopt the later ISIN")
	require.Contains(t, reused.Note, "NOT merged")

	gone := byTicker["GONE"]
	require.Equal(t, preisin.Dead, gone.Status)
	require.True(t, gone.Synthetic)
	require.Equal(t, "X-NSE-GONE", gone.ISIN)
}

// TestSyntheticCannotBeMistakenForARealISIN. ISO 6166 opens with two letters
// of country code, so nothing real starts "X-". The point is not tidiness: a
// fabricated identifier shaped like an ISIN would travel through symbols,
// symbol_links and every join with nothing marking it inferred, and no later
// reader could tell which companies this project invented.
func TestSyntheticCannotBeMistakenForARealISIN(t *testing.T) {
	s := preisin.Synthetic("tatasteel")
	require.Equal(t, "X-NSE-TATASTEEL", s, "tickers normalise to upper case")
	require.True(t, preisin.IsSynthetic(s))

	for _, real := range []string{"INE081A01012", "INE081A01020", "INF204KB14I2", "IN9081A01010"} {
		require.False(t, preisin.IsSynthetic(real), "%s is a real ISIN", real)
		require.NotEqual(t, s, real)
	}
	require.NotContains(t, s[:2], "IN")
}

// TestUnknownDatesNeverBridge. gapSessions returns a huge number for a date the
// calendar does not contain, and the resolver must treat that as "too far"
// rather than "zero". A missing date meaning "adjacent" is how an archive hole
// would quietly merge two companies.
func TestUnknownDatesNeverBridge(t *testing.T) {
	gap := sessionGap(cal(market.Day(2011, 7, 4)))
	got, sum := preisin.Resolve([]preisin.Observation{
		{Ticker: "HOLE", LastPreISIN: market.Day(1999, 1, 1),
			FirstISINEra: market.Day(2011, 7, 4), ISINEraISIN: "INE111A01011"},
	}, gap, preisin.MaxBridgeGapSessions)

	require.Equal(t, preisin.Summary{Quarantined: 1}, sum)
	require.True(t, got[0].Synthetic)
}

// TestSessionGapFuncCountsSessionsNotDays. Weekday arithmetic is wrong on this
// calendar in the DANGEROUS direction: a run of holidays shrinks an apparent
// gap, and a shrunken gap is what bridges two instruments that should not be
// joined.
func TestSessionGapFuncCountsSessionsNotDays(t *testing.T) {
	// A Thursday, then a long exchange holiday, then the next session eleven
	// calendar days later but only ONE session later.
	sessions := []time.Time{
		market.Day(2011, 6, 9), market.Day(2011, 6, 20), market.Day(2011, 6, 21),
	}
	gap := preisin.SessionGapFunc(sessions)

	require.Equal(t, 1, gap(market.Day(2011, 6, 9), market.Day(2011, 6, 20)),
		"eleven calendar days apart, one session apart")
	require.Equal(t, 2, gap(market.Day(2011, 6, 9), market.Day(2011, 6, 21)))
	require.Equal(t, 0, gap(market.Day(2011, 6, 9), market.Day(2011, 6, 9)))

	// A date the archive never held must read as far away, never as adjacent.
	require.Greater(t, gap(market.Day(2011, 6, 10), market.Day(2011, 6, 21)), 1000,
		"an unknown session must never look close enough to bridge")
	// And time must not run backwards into a bridge either.
	require.Greater(t, gap(market.Day(2011, 6, 21), market.Day(2011, 6, 9)), 1000)
}
