package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/engine"
	"github.com/HardikSJain/verdict-machine/internal/risk"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

var (
	d1 = day(2026, 9, 1)
	d2 = day(2026, 9, 2)
	d3 = day(2026, 9, 3)
	d4 = day(2026, 9, 4)
)

// scripted is a strategy that emits whatever a test tells it to, on the
// sessions the test names. Real strategies arrive in the next piece; what is
// under test here is the LOOP.
type scripted struct {
	name string
	on   map[string][]risk.Intent
	seen []time.Time
}

func (s *scripted) Name() string { return s.name }

func (s *scripted) Decide(_ context.Context, sess engine.Session) ([]risk.Intent, error) {
	s.seen = append(s.seen, sess.Date)
	return s.on[sess.Date.Format(time.DateOnly)], nil
}

// Fixtures name entities by the same small integers their bars use, because an
// intent is identified by entity id and only labelled by ticker.
var entityOf = map[string]int64{"AAA": 1, "BBB": 2, "CCC": 3, "OLDNAME": 1, "NEWNAME": 1}

func buyIntent(scrip string, qty int64, price float64) risk.Intent {
	return risk.Intent{EntityID: entityOf[scrip], Scrip: scrip, Side: cost.Buy,
		Product: cost.EquityDelivery, Quantity: qty, Price: price}
}

func sellIntent(scrip string, qty int64, price float64) risk.Intent {
	return risk.Intent{EntityID: entityOf[scrip], Scrip: scrip, Side: cost.Sell,
		Product: cost.EquityDelivery, Quantity: qty, Price: price}
}

// harness wires a fixture market to a paper broker, a real risk gate and a
// scripted strategy. Nothing here is a mock of the cost model: fills are
// priced by the shipping schedule, because a backtest whose costs are faked is
// answering a question nobody asked.
func harness(t *testing.T, f *engine.Fixture, s engine.Strategy, l risk.Limits) *engine.Engine {
	t.Helper()
	sched, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	broker, err := engine.NewPaper(sched, 0)
	require.NoError(t, err)
	g, err := risk.NewGate(l, sched)
	require.NoError(t, err)
	e, err := engine.New(f, broker, g, s)
	require.NoError(t, err)
	return e
}

func looseLimits() risk.Limits {
	l := risk.DefaultLimits()
	l.MinNotional = 1000 // override the derivation; sizing is not what these test
	l.MaxStockFraction = 1
	return l
}

// TestOrdersFillAtTheNextOpenNotTheDecidingClose is the rule the whole engine
// exists to enforce, and the fixture is built so that getting it wrong is
// impossible to miss: the deciding session closes at 100 and the next session
// opens at 150.
//
// A strategy that could trade at the close it just read would show a fill at
// 100. Every backtest that has ever flattered itself has done so through some
// version of that one line.
func TestOrdersFillAtTheNextOpenNotTheDecidingClose(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2, d3}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 98, Close: 100}).
		AddBar(d2, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 150, Close: 155}).
		AddBar(d3, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 160, Close: 162})

	s := &scripted{name: "one-buy", on: map[string][]risk.Intent{
		d1.Format(time.DateOnly): {buyIntent("AAA", 20, 100)},
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)

	require.Len(t, res.Fills, 1)
	fill := res.Fills[0]
	require.Equal(t, d2, fill.Date, "the fill lands on the session AFTER the decision")
	require.Equal(t, d1, fill.DecidedOn)
	require.Equal(t, 150.0, fill.Price, "at that session's OPEN, not the 100 close it was decided on")
	require.NotEqual(t, 100.0, fill.Price)
}

// TestOrdersDecidedOnTheFinalSessionNeverFill: they were never given an open
// to trade at, and inventing one would be a look-ahead of exactly one day.
func TestOrdersDecidedOnTheFinalSessionNeverFill(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 98, Close: 100}).
		AddBar(d2, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 101, Close: 102})

	s := &scripted{name: "late", on: map[string][]risk.Intent{
		d2.Format(time.DateOnly): {buyIntent("AAA", 20, 102)},
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)
	require.Empty(t, res.Fills)
	require.Empty(t, res.Unfilled, "it is not unfilled either; it was simply never submitted to an open")
	require.Equal(t, []time.Time{d1, d2}, s.seen, "and the strategy still saw every session")
}

// TestChargesLeaveCashOnBothSides. A buy costs turnover PLUS charges and a
// sell returns turnover MINUS charges. Netting charges into the fill price
// instead would make every cost basis a fiction and understate the drag that
// this whole project is trying to measure.
func TestChargesLeaveCashOnBothSides(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2, d3, d4}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(d2, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(d3, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(d4, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100})

	s := &scripted{name: "round-trip", on: map[string][]risk.Intent{
		d1.Format(time.DateOnly): {buyIntent("AAA", 100, 100)},  // fills d2 at 100
		d2.Format(time.DateOnly): {sellIntent("AAA", 100, 100)}, // fills d3 at 100
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)
	require.Len(t, res.Fills, 2)

	// Price never moved, so every rupee lost is a charge and nothing else.
	require.Empty(t, res.Final.Positions)
	lost := 100_000 - res.FinalCash
	require.InDelta(t, res.TotalCosts, lost, 1e-6,
		"a flat round trip must lose exactly its charges")
	require.Greater(t, res.TotalCosts, 0.0)

	// And the DP charge is in there: a 10,000 sell at Zerodha pays 15.34 of it.
	var dp float64
	for _, fl := range res.Fills {
		dp += fl.Charges.DPCharge
	}
	require.InDelta(t, 15.34, dp, 1e-6)
}

// TestDPChargedOncePerScripPerDayAcrossClips: exiting one position in three
// orders on one session must pay the depository once, not three times. At the
// Rs10,000 position size the design sets as its floor, getting this wrong
// triples a fee that is already a third of the whole round-trip cost.
func TestDPChargedOncePerScripPerDayAcrossClips(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2, d3}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(d2, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(d3, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100})

	s := &scripted{name: "clipped-exit", on: map[string][]risk.Intent{
		d1.Format(time.DateOnly): {buyIntent("AAA", 300, 100)},
		d2.Format(time.DateOnly): {
			sellIntent("AAA", 100, 100), sellIntent("AAA", 100, 100), sellIntent("AAA", 100, 100),
		},
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)

	var dp float64
	var sells int
	for _, fl := range res.Fills {
		dp += fl.Charges.DPCharge
		if fl.Side == cost.Sell {
			sells++
		}
	}
	require.Equal(t, 3, sells, "three separate sell fills")
	require.InDelta(t, 15.34, dp, 1e-6, "and exactly one DP charge between them")
}

// TestRiskRejectionsAreRecordedNotDropped. An intent the gate refused has to
// reach the report, or a backtest quietly becomes a different strategy from
// the one that was written.
func TestRiskRejectionsAreRecordedNotDropped(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(d2, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100})

	l := looseLimits()
	l.MinNotional = 50_000 // well above the intent below

	s := &scripted{name: "too-small", on: map[string][]risk.Intent{
		d1.Format(time.DateOnly): {buyIntent("AAA", 10, 100)},
	}}
	res, err := harness(t, f, s, l).Run(context.Background(), 100_000)
	require.NoError(t, err)
	require.Empty(t, res.Fills)
	require.Len(t, res.Rejected, 1)
	require.Equal(t, risk.RuleMinNotional, res.Rejected[0].Rule)
	require.Equal(t, 50_000.0, res.MinNotional)
	require.False(t, res.SlippageModelled)
}

// TestUnfilledWhenTheNameDoesNotTrade: an order that reaches an open with no
// price there is reported, never silently abandoned. A backtest that drops
// them is choosing not to take trades it said it wanted, and choosing
// invisibly.
func TestUnfilledWhenTheNameDoesNotTrade(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2, d3}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		// AAA does not trade on d2 at all.
		AddBar(d2, engine.Bar{EntityID: 2, Scrip: "BBB", Open: 50, Close: 50}).
		AddBar(d3, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100})

	s := &scripted{name: "halted", on: map[string][]risk.Intent{
		d1.Format(time.DateOnly): {buyIntent("AAA", 100, 100)},
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)
	require.Empty(t, res.Fills)
	require.Len(t, res.Unfilled, 1)
	require.Equal(t, "AAA", res.Unfilled[0].Scrip)
	require.Contains(t, res.Unfilled[0].Reason, "did not trade")
}

// TestABuyBeyondCashIsUnfilledNotLeverage. The play tier is cash equity, so a
// book that could go short of cash would be measuring a strategy nobody could
// have run.
//
// It is reported as unfilled rather than fatal, because sizing happens on a
// close and filling happens at the next open: an adverse overnight gap can
// make a correctly sized order unaffordable by morning, which is an ordinary
// event a real broker answers by rejecting the order. A run that aborted there
// could not model its own most common failure.
func TestABuyBeyondCashIsUnfilledNotLeverage(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(d2, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100})

	s := &scripted{name: "greedy", on: map[string][]risk.Intent{
		d1.Format(time.DateOnly): {buyIntent("AAA", 1000, 100)}, // 100,000 plus charges
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)
	require.Empty(t, res.Fills)
	require.Len(t, res.Unfilled, 1)
	require.ErrorIs(t, engine.ErrInsufficientCash, engine.ErrInsufficientCash)
	require.Contains(t, res.Unfilled[0].Reason, "insufficient cash")
	require.Contains(t, res.Unfilled[0].Reason, "the book holds")
	require.Equal(t, 100_000.0, res.FinalCash, "and not a rupee of it was spent")
}

// TestEquityCurveMarksAtEveryClose, including a session where a held name did
// not trade -- it keeps its last mark instead of valuing at zero, which would
// print a fake drawdown and a fake recovery on either side of a halt.
func TestEquityCurveMarksAtEveryClose(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2, d3, d4}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(d2, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 120}).
		// d3: AAA does not trade.
		AddBar(d3, engine.Bar{EntityID: 2, Scrip: "BBB", Open: 10, Close: 10}).
		AddBar(d4, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 130, Close: 130})

	s := &scripted{name: "hold", on: map[string][]risk.Intent{
		d1.Format(time.DateOnly): {buyIntent("AAA", 100, 100)},
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)
	require.Len(t, res.Equity, 4)

	// Bought at d2's open of 100, marked at d2's close of 120.
	require.InDelta(t, 12_000, res.Equity[1].Equity-res.Equity[1].Cash, 1e-6)
	// d3 has no bar for AAA: the mark holds at 120, not zero.
	require.InDelta(t, res.Equity[1].Equity, res.Equity[2].Equity, 1e-6,
		"a halted name must not print a 100% drawdown and a 100% recovery")
	require.Equal(t, 1, res.Equity[2].Held)
	// d4 marks at 130.
	require.InDelta(t, 13_000, res.Equity[3].Equity-res.Equity[3].Cash, 1e-6)
}

// TestUnverifiedChargesArePropagatedToTheResult. The cost model reports which
// rates nobody has reconciled; the engine has to carry that all the way to the
// report, or a backtest on unverified rates looks exactly like one that is not.
func TestUnverifiedChargesArePropagatedToTheResult(t *testing.T) {
	// 2015 predates uniform stamp duty and GST entirely.
	old1, old2, old3 := day(2015, 6, 1), day(2015, 6, 2), day(2015, 6, 3)
	f := engine.NewFixture([]time.Time{old1, old2, old3}).
		AddBar(old1, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(old2, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100}).
		AddBar(old3, engine.Bar{EntityID: 1, Scrip: "AAA", Open: 100, Close: 100})

	s := &scripted{name: "old", on: map[string][]risk.Intent{
		old1.Format(time.DateOnly): {buyIntent("AAA", 200, 100)},
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)
	require.Len(t, res.Fills, 1)
	require.NotEmpty(t, res.Unverified)
	require.Contains(t, res.Unverified, cost.StampDuty)
	require.Contains(t, res.Unverified, cost.GST)
}

// TestSlippageMovesTheFillAgainstTheTrader on both sides. A model that applied
// it to one leg would flatter every round trip by half.
func TestSlippageMovesTheFillAgainstTheTrader(t *testing.T) {
	sched, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	broker, err := engine.NewPaper(sched, 50) // 50 bps
	require.NoError(t, err)

	orders := []engine.Order{
		{EntityID: 1, Scrip: "AAA", Side: cost.Buy, Product: cost.EquityDelivery, Quantity: 10},
		{EntityID: 2, Scrip: "BBB", Side: cost.Sell, Product: cost.EquityDelivery, Quantity: 10},
	}
	ex, err := broker.Execute(context.Background(), d2, orders, map[int64]float64{1: 100, 2: 100})
	require.NoError(t, err)
	require.Len(t, ex.Fills, 2)

	byScrip := map[string]float64{}
	for _, f := range ex.Fills {
		byScrip[f.Scrip] = f.Price
	}
	require.InDelta(t, 100.5, byScrip["AAA"], 1e-9, "the buyer pays up")
	require.InDelta(t, 99.5, byScrip["BBB"], 1e-9, "the seller receives less")
}

// TestFixtureReturnsKeepTheThreeWaySplit. The fence's contract has to survive
// the seam, or a strategy tested against the fixture would meet a different
// interface from the one production hands it.
func TestFixtureReturnsKeepTheThreeWaySplit(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2, d3}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "AAA", Close: 100}).
		AddBar(d3, engine.Bar{EntityID: 1, Scrip: "AAA", Close: 110}).
		AddBar(d1, engine.Bar{EntityID: 2, Scrip: "BBB", Close: 50}).
		AddBar(d3, engine.Bar{EntityID: 2, Scrip: "BBB", Close: 60}).
		AddBar(d3, engine.Bar{EntityID: 3, Scrip: "CCC", Close: 10}).
		Refuse(2, "succession boundary in the window")

	rs, err := f.Returns(context.Background(), []int64{1, 2, 3}, d1, d3)
	require.NoError(t, err)
	require.InDelta(t, 0.10, rs.Priced[1], 1e-9)
	require.Contains(t, rs.Refused, int64(2))
	require.NotContains(t, rs.Priced, int64(2), "a refused name never also carries a number")
	require.Contains(t, rs.Absent, int64(3), "no bar at the near endpoint")
}

// TestEngineRefusesAnIncompleteWiring: every seam is required, and a nil one
// should fail at construction rather than at the first session.
func TestEngineRefusesAnIncompleteWiring(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2})
	_, err := engine.New(f, nil, nil, nil)
	require.Error(t, err)

	sched, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	broker, err := engine.NewPaper(sched, 0)
	require.NoError(t, err)
	g, err := risk.NewGate(looseLimits(), sched)
	require.NoError(t, err)

	e, err := engine.New(engine.NewFixture([]time.Time{d1}), broker, g, &scripted{name: "x"})
	require.NoError(t, err)
	_, err = e.Run(context.Background(), 1000)
	require.Error(t, err, "one session cannot fill anything")
	require.Contains(t, err.Error(), "at least two sessions")
}

// TestARenamedHoldingCanStillBeSold is the regression for a bug that made the
// first backtest meaningless, and it was invisible in every unit test because
// no fixture had ever renamed anything.
//
// The engine used to recover an intent's entity by matching its ticker against
// the session's bars. Tickers change while a position is held -- RNAM became
// NAM-INDIA, ADANIGAS became ATGL, IBSEC became IBVENTURES became DHANI, all
// under one unchanged ISIN -- and after a rename no bar carried the string the
// holding remembered. The sell never became an order. The position stuck in the
// book permanently, marked at its last known price, inflating equity, while
// every later exit attempt failed silently and re-fired the next session.
//
// The fix is that an intent carries the entity id and the ticker is only a
// label. This test renames the instrument between the buy and the sell, which
// the old code could not survive.
func TestARenamedHoldingCanStillBeSold(t *testing.T) {
	f := engine.NewFixture([]time.Time{d1, d2, d3, d4}).
		AddBar(d1, engine.Bar{EntityID: 1, Scrip: "OLDNAME", Open: 100, Close: 100}).
		AddBar(d2, engine.Bar{EntityID: 1, Scrip: "OLDNAME", Open: 100, Close: 100}).
		// The exchange starts printing a different ticker for the same entity.
		AddBar(d3, engine.Bar{EntityID: 1, Scrip: "NEWNAME", Open: 100, Close: 100}).
		AddBar(d4, engine.Bar{EntityID: 1, Scrip: "NEWNAME", Open: 100, Close: 100})

	s := &scripted{name: "renamed", on: map[string][]risk.Intent{
		d1.Format(time.DateOnly): {buyIntent("OLDNAME", 100, 100)},
		// Sold under the name it was BOUGHT as, which is what a portfolio
		// remembers and what the strategy therefore emits.
		d3.Format(time.DateOnly): {{
			EntityID: 1, Scrip: "OLDNAME", Side: cost.Sell,
			Product: cost.EquityDelivery, Quantity: 100, Price: 100,
		}},
	}}
	res, err := harness(t, f, s, looseLimits()).Run(context.Background(), 100_000)
	require.NoError(t, err)

	require.Len(t, res.Fills, 2, "the buy and the sell both filled across the rename")
	require.Empty(t, res.Unfilled)
	require.Empty(t, res.Final.Positions,
		"a renamed holding must not be stuck in the book forever")

	// And the fill carries the CURRENT ticker, so a report names the company as
	// it trades today rather than as it traded when it was bought.
	var sellFill engine.Fill
	for _, fl := range res.Fills {
		if fl.Side == cost.Sell {
			sellFill = fl
		}
	}
	require.Equal(t, "NEWNAME", sellFill.Scrip)
}
