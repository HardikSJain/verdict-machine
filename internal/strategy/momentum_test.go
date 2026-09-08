package strategy_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/engine"
	"github.com/HardikSJain/verdict-machine/internal/risk"
	"github.com/HardikSJain/verdict-machine/internal/strategy"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// monthlySessions is a synthetic calendar: weekdays across a span, which is
// enough to exercise month-end detection without pretending to be NSE's
// holiday list.
func weekdays(from, to time.Time) []time.Time {
	var out []time.Time
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		out = append(out, d)
	}
	return out
}

// trending builds a fixture where each entity's close grows at its own steady
// rate, so the 12-1 ranking is known in advance: the fastest grower ranks
// first. Entity id n grows at n basis points a session.
func trending(sessions []time.Time, n int) *engine.Fixture {
	f := engine.NewFixture(sessions)
	for _, d := range sessions {
		for i := 1; i <= n; i++ {
			id := int64(i)
			elapsed := float64(daysSince(sessions[0], d))
			px := 100 * (1 + float64(i)*0.0004*elapsed)
			f.AddBar(d, engine.Bar{
				EntityID: id, Scrip: scripName(i),
				Open: px, High: px, Low: px, Close: px,
			})
		}
	}
	return f
}

func daysSince(a, b time.Time) int { return int(b.Sub(a).Hours() / 24) }

func scripName(i int) string { return string(rune('A'+(i-1)%26)) + string(rune('0'+i/26)) }

func TestMomentumRanksByTheSkippedWindowAndHoldsTopN(t *testing.T) {
	sessions := weekdays(day(2024, 1, 1), day(2025, 6, 30))
	f := trending(sessions, 10)

	m, err := strategy.NewMomentum(3, 12, 1, strategy.AlwaysOn{})
	require.NoError(t, err)

	sched, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	broker, err := engine.NewPaper(sched, 0)
	require.NoError(t, err)
	l := risk.DefaultLimits()
	l.MinNotional = 1000
	l.MaxStockFraction = 1
	g, err := risk.NewGate(l, sched)
	require.NoError(t, err)
	e, err := engine.New(f, broker, g, m)
	require.NoError(t, err)

	res, err := e.Run(context.Background(), 1_000_000)
	require.NoError(t, err)

	j := m.Journal()
	require.Greater(t, j.Rebalances, 4, "eighteen months must produce several month ends")
	require.NotEmpty(t, j.Rankings)

	// Entity 10 grows fastest, so it must be selected at every rebalance that
	// had a full formation window.
	last := j.Rankings[len(j.Rankings)-1]
	require.Len(t, last.Selected, 3)
	require.Equal(t, scripName(10), last.Selected[0],
		"the fastest riser must rank first under 12-1 momentum")
	require.Equal(t, scripName(9), last.Selected[1])
	require.Equal(t, scripName(8), last.Selected[2])

	require.NotEmpty(t, res.Fills)
	require.Equal(t, 3, len(res.Final.Positions), "equal-weight top 3 held at the end")
}

// TestRebalanceDecidesOnTheLastSessionOfTheMonth. The design says the book
// turns over at the open of the first trading day of a month on the prior
// close's signals, so the DECISION has to land on the last session of the
// previous month and the engine's next-open rule does the rest.
func TestRebalanceDecidesOnTheLastSessionOfTheMonth(t *testing.T) {
	sessions := weekdays(day(2024, 1, 1), day(2025, 6, 30))
	f := trending(sessions, 5)
	m, err := strategy.NewMomentum(2, 12, 1, strategy.AlwaysOn{})
	require.NoError(t, err)

	sched, _ := cost.NewSchedule(cost.Zerodha)
	broker, _ := engine.NewPaper(sched, 0)
	l := risk.DefaultLimits()
	l.MinNotional = 1000
	l.MaxStockFraction = 1
	g, _ := risk.NewGate(l, sched)
	e, _ := engine.New(f, broker, g, m)
	res, err := e.Run(context.Background(), 1_000_000)
	require.NoError(t, err)

	for _, fill := range res.Fills {
		// Every fill was decided on a session whose month differs from the
		// fill's own: the last of one month, filling on the first of the next.
		require.NotEqual(t, fill.DecidedOn.Month(), fill.Date.Month(),
			"a fill decided inside its own month means the month-end rule broke")
		require.True(t, fill.DecidedOn.Before(fill.Date))
	}
	require.NotEmpty(t, res.Fills)
}

// TestRefusedReturnsAreExcludedAndCounted: a name the succession fence refuses
// must not be ranked, and must not vanish silently either. Ranking it would
// mean ranking on a split-sized fake return; dropping it quietly would mean a
// backtest choosing from a smaller universe than it reports.
func TestRefusedReturnsAreExcludedAndCounted(t *testing.T) {
	sessions := weekdays(day(2024, 1, 1), day(2025, 6, 30))
	f := trending(sessions, 5).Refuse(5, "succession boundary inside the window")

	m, err := strategy.NewMomentum(2, 12, 1, strategy.AlwaysOn{})
	require.NoError(t, err)
	sched, _ := cost.NewSchedule(cost.Zerodha)
	broker, _ := engine.NewPaper(sched, 0)
	l := risk.DefaultLimits()
	l.MinNotional = 1000
	l.MaxStockFraction = 1
	g, _ := risk.NewGate(l, sched)
	e, _ := engine.New(f, broker, g, m)
	_, err = e.Run(context.Background(), 1_000_000)
	require.NoError(t, err)

	j := m.Journal()
	require.Greater(t, j.ReturnsRefused, 0)
	require.Equal(t, j.Rebalances, j.RefusedByScrip[scripName(5)],
		"the refused name is refused at every rebalance and counted at each")
	for _, r := range j.Rankings {
		require.NotContains(t, r.Selected, scripName(5),
			"the fastest riser is refused, so it must never be selected")
	}
}

// TestRiskOffLiquidatesOnAnySessionNotOnlyMonthEnd. The design says the book
// goes to cash when the trend breaks and waits there; a filter that acted only
// at month end would hold through most of a drawdown it had already flagged.
func TestRiskOffLiquidatesOnAnySessionNotOnlyMonthEnd(t *testing.T) {
	sessions := weekdays(day(2024, 1, 1), day(2025, 6, 30))
	f := trending(sessions, 5)

	flip := day(2025, 5, 14) // a Wednesday, mid-month
	m, err := strategy.NewMomentum(2, 12, 1, &switchAt{on: true, off: flip})
	require.NoError(t, err)
	sched, _ := cost.NewSchedule(cost.Zerodha)
	broker, _ := engine.NewPaper(sched, 0)
	l := risk.DefaultLimits()
	l.MinNotional = 1000
	l.MaxStockFraction = 1
	g, _ := risk.NewGate(l, sched)
	e, _ := engine.New(f, broker, g, m)
	res, err := e.Run(context.Background(), 1_000_000)
	require.NoError(t, err)

	require.Empty(t, res.Final.Positions, "risk-off must end the run in cash")
	require.Greater(t, m.Journal().Liquidations, 0)

	var lastSell time.Time
	for _, fl := range res.Fills {
		if fl.Side == cost.Sell {
			lastSell = fl.Date
		}
	}
	require.False(t, lastSell.IsZero())
	require.NotEqual(t, 1, lastSell.Day(),
		"the exit landed mid-month, not at the next month's open")
	require.True(t, lastSell.After(flip) && lastSell.Before(flip.AddDate(0, 0, 5)),
		"and within a session of the flip, at %s", lastSell.Format(time.DateOnly))
}

// switchAt is risk-on until a date and off from it, so a test can put the flip
// exactly where it wants without building a price series that produces one.
type switchAt struct {
	on  bool
	off time.Time
}

func (s *switchAt) Name() string { return "switch-at" }

func (s *switchAt) RiskOn(_ context.Context, sess engine.Session) (bool, string, error) {
	if !sess.Date.Before(s.off) {
		return false, "test flip", nil
	}
	return true, "test", nil
}

// TestTrendFilterIsRiskOffWhileWarmingUp. Not enough history is not evidence
// that the trend is up, and a filter that defaulted to invested would be
// silently absent over exactly the stretch nobody checked.
func TestTrendFilterIsRiskOffWhileWarmingUp(t *testing.T) {
	sessions := weekdays(day(2025, 1, 1), day(2025, 6, 30))
	f := trending(sessions, 3)
	tf, err := strategy.NewTrendFilter(1, "PROXY", 200)
	require.NoError(t, err)

	on, why, err := tf.RiskOn(context.Background(), engine.Session{
		Date: sessions[20], Market: f,
	})
	require.NoError(t, err)
	require.False(t, on)
	require.Contains(t, why, "warming up")
}

// TestTrendFilterFollowsTheMean over a series that rises then falls.
func TestTrendFilterFollowsTheMean(t *testing.T) {
	sessions := weekdays(day(2024, 1, 1), day(2025, 6, 30))
	f := engine.NewFixture(sessions)
	// Rise for the first two thirds, then fall hard.
	turn := len(sessions) * 2 / 3
	for i, d := range sessions {
		px := 100 + float64(i)
		if i > turn {
			px = 100 + float64(turn) - float64(i-turn)*3
		}
		f.AddBar(d, engine.Bar{EntityID: 1, Scrip: "PROXY", Open: px, Close: px})
	}
	tf, err := strategy.NewTrendFilter(1, "PROXY", 50)
	require.NoError(t, err)

	on, why, err := tf.RiskOn(context.Background(), engine.Session{Date: sessions[turn], Market: f})
	require.NoError(t, err)
	require.True(t, on, "at the peak the close is above its own mean: %s", why)

	on, why, err = tf.RiskOn(context.Background(), engine.Session{Date: sessions[len(sessions)-1], Market: f})
	require.NoError(t, err)
	require.False(t, on, "after the fall it is below: %s", why)
	require.Contains(t, why, "below its 50-day mean")
}

// TestTrendFilterRefusesAStaleProxy: a proxy that did not trade today cannot
// drive today's rule, because a stale close is exactly what a halted or
// delisted proxy produces and it would freeze the filter in its last state.
func TestTrendFilterRefusesAStaleProxy(t *testing.T) {
	sessions := weekdays(day(2024, 1, 1), day(2025, 6, 30))
	f := engine.NewFixture(sessions)
	for i, d := range sessions[:len(sessions)-5] {
		px := 100 + float64(i)
		f.AddBar(d, engine.Bar{EntityID: 1, Scrip: "PROXY", Open: px, Close: px})
	}
	tf, err := strategy.NewTrendFilter(1, "PROXY", 50)
	require.NoError(t, err)

	on, why, err := tf.RiskOn(context.Background(), engine.Session{
		Date: sessions[len(sessions)-1], Market: f,
	})
	require.NoError(t, err)
	require.False(t, on)
	require.Contains(t, why, "did not trade")
}

// TestMomentumRefusesNonsenseParameters, in particular a formation window that
// does not exceed the skip -- which would rank on a window of zero or negative
// length and produce a silent nonsense ordering.
func TestMomentumRefusesNonsenseParameters(t *testing.T) {
	_, err := strategy.NewMomentum(0, 12, 1, strategy.AlwaysOn{})
	require.Error(t, err)
	_, err = strategy.NewMomentum(20, 1, 1, strategy.AlwaysOn{})
	require.Error(t, err)
	_, err = strategy.NewMomentum(20, 12, -1, strategy.AlwaysOn{})
	require.Error(t, err)
	_, err = strategy.NewMomentum(20, 12, 1, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "AlwaysOn")

	_, err = strategy.NewTrendFilter(0, "X", 200)
	require.Error(t, err)
	_, err = strategy.NewTrendFilter(1, "X", 1)
	require.Error(t, err)
}
