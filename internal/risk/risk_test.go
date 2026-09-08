package risk_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/risk"
)

var testDay = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

func sched(t *testing.T, b cost.Broker) *cost.Schedule {
	t.Helper()
	s, err := cost.NewSchedule(b)
	require.NoError(t, err)
	return s
}

func gate(t *testing.T, l risk.Limits, b cost.Broker) *risk.Gate {
	t.Helper()
	g, err := risk.NewGate(l, sched(t, b))
	require.NoError(t, err)
	return g
}

// id turns a fixture's scrip label into a stable entity id, because an intent
// is identified by entity and only labelled by ticker.
func id(scrip string) int64 {
	var n int64
	for _, r := range scrip {
		n = n*31 + int64(r)
	}
	if n < 0 {
		n = -n
	}
	return n + 1
}

func buy(scrip string, notional float64) risk.Intent {
	return risk.Intent{EntityID: id(scrip), Scrip: scrip, Side: cost.Buy,
		Product: cost.EquityDelivery, Quantity: int64(notional), Price: 1}
}

func sell(scrip string, notional float64) risk.Intent {
	return risk.Intent{EntityID: id(scrip), Scrip: scrip, Side: cost.Sell,
		Product: cost.EquityDelivery, Quantity: int64(notional), Price: 1}
}

// TestMinNotionalIsDerivedFromTheCharges is the change this package makes to
// the design, and the reason it was worth making.
//
// The design fixed the floor at "roughly Rs10k at current charges". Solved
// from the charges it is Rs5,600 at Zerodha -- the design was conservative by
// about 80%, which with 20 positions is the difference between needing
// Rs2,00,000 of algo capital and needing Rs1,12,000. At Angel One the same
// rule gives Rs25,600, so a hardcoded 10,000 would have been wrong in both
// directions depending on the broker.
//
// The magic numbers are asserted alongside the PROPERTY that produced them, so
// a charge change moves them and fails loudly rather than leaving a stale
// constant that still looks deliberate.
func TestMinNotionalIsDerivedFromTheCharges(t *testing.T) {
	const cap = 0.005
	for _, tc := range []struct {
		broker cost.Broker
		want   float64
	}{
		{cost.Zerodha, 5600},
		{cost.AngelOne, 25600},
	} {
		t.Run(string(tc.broker), func(t *testing.T) {
			s := sched(t, tc.broker)
			got, err := risk.MinNotional(s, testDay, cost.EquityDelivery, cap, 0)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)

			at, err := risk.RoundTripCost(s, testDay, cost.EquityDelivery, got, 0)
			require.NoError(t, err)
			require.LessOrEqual(t, at, cap, "the floor itself must clear the cap")

			below, err := risk.RoundTripCost(s, testDay, cost.EquityDelivery, got-100, 0)
			require.NoError(t, err)
			require.Greater(t, below, cap, "and one step below it must not, or the floor is not minimal")
		})
	}
}

// TestTheCostFloorIsSTTAndNoSizeEscapesIt is worth its own test because it
// bounds what any risk rule can achieve.
//
// STT is 0.1% a side on delivery and scales perfectly with notional, so
// round-trip cost asymptotes near 0.2% however large the position. A 0.5% cap
// is comfortably reachable; a 0.25% cap needs ten times the position; a 0.2%
// cap is unreachable at any size, and the gate says so rather than returning
// an absurd floor.
func TestTheCostFloorIsSTTAndNoSizeEscapesIt(t *testing.T) {
	s := sched(t, cost.Zerodha)

	loose, err := risk.MinNotional(s, testDay, cost.EquityDelivery, 0.005, 0)
	require.NoError(t, err)
	tight, err := risk.MinNotional(s, testDay, cost.EquityDelivery, 0.0025, 0)
	require.NoError(t, err)
	require.Greater(t, tight, loose*5,
		"halving the cost budget costs far more than twice the size, because STT does not scale away")

	big, err := risk.RoundTripCost(s, testDay, cost.EquityDelivery, 10_000_000, 0)
	require.NoError(t, err)
	require.Greater(t, big, 0.002, "a crore round trip still pays 0.2% in STT alone")

	_, err = risk.MinNotional(s, testDay, cost.EquityDelivery, 0.002, 0)
	require.Error(t, err, "a cap at the STT floor is unreachable and must not silently return a number")
	require.Contains(t, err.Error(), "unreachable")
}

// TestSellsAreNeverBlockedByACap is the package's first principle.
//
// Every cap is engaged at once -- the book is full, over its stock limit, over
// its notional limit and past its daily loss cap -- and a sell still passes.
// A gate that can refuse an exit can trap the book in the position it has
// decided it wants out of, and that failure has no bound.
func TestSellsAreNeverBlockedByACap(t *testing.T) {
	l := risk.DefaultLimits()
	l.MaxPositions = 1
	l.MaxBookNotional = 10_000
	l.DailyLossCap = 1_000
	g := gate(t, l, cost.Zerodha)

	book := risk.Book{
		Equity:    50_000,
		Positions: map[string]float64{strconv.FormatInt(id("HELD"), 10): 49_000},
		DayPnL:    -25_000, // far past the cap
	}
	d, err := g.Check(testDay, book, []risk.Intent{sell("HELD", 49_000)})
	require.NoError(t, err)
	require.Len(t, d.Accepted, 1)
	require.Empty(t, d.Rejected)

	// And the same batch's buy is refused, so the test is not passing because
	// nothing is enforced.
	d2, err := g.Check(testDay, book, []risk.Intent{buy("NEW", 20_000)})
	require.NoError(t, err)
	require.Empty(t, d2.Accepted)
	require.Len(t, d2.Rejected, 1)
}

// TestKillSwitchStopsSellsToo records a DECISION, not a derivation, and it is
// the one rule here that overrides the never-block-an-exit principle.
//
// The reasoning: a kill switch means the machine is not to be trusted, and a
// machine that is not to be trusted should not be choosing when to liquidate
// either. The human keeps a Kite login and can sell by hand, so the exit is
// never actually unavailable -- only automation of it is. The design does not
// specify this, so it is open to being overruled.
func TestKillSwitchStopsSellsToo(t *testing.T) {
	g := gate(t, risk.DefaultLimits(), cost.Zerodha)
	book := risk.Book{
		Equity:     100_000,
		Positions:  map[string]float64{strconv.FormatInt(id("HELD"), 10): 50_000},
		KillSwitch: "manual halt while the bhavcopy loader is being fixed",
	}
	d, err := g.Check(testDay, book, []risk.Intent{buy("NEW", 20_000), sell("HELD", 50_000)})
	require.NoError(t, err)
	require.Empty(t, d.Accepted)
	require.Len(t, d.Rejected, 2)
	for _, r := range d.Rejected {
		require.Equal(t, risk.RuleKillSwitch, r.Rule)
		require.Contains(t, r.Detail, "bhavcopy loader")
	}
}

// TestBelowFloorIsRejectedNotRoundedUp: the design is explicit that an intent
// under the floor is refused rather than resized, because rounding it up would
// size a position off a risk rule instead of off the strategy.
func TestBelowFloorIsRejectedNotRoundedUp(t *testing.T) {
	g := gate(t, risk.DefaultLimits(), cost.Zerodha)
	book := risk.Book{Equity: 500_000, Positions: map[string]float64{}}

	small := buy("TINY", 3_000)
	d, err := g.Check(testDay, book, []risk.Intent{small})
	require.NoError(t, err)
	require.Empty(t, d.Accepted)
	require.Len(t, d.Rejected, 1)
	require.Equal(t, risk.RuleMinNotional, d.Rejected[0].Rule)
	require.Equal(t, small.Quantity, d.Rejected[0].Intent.Quantity,
		"the rejected intent comes back unmodified; nothing was resized")
	require.Equal(t, 5600.0, d.MinNotional)
	require.False(t, d.SlippageModelled,
		"with no measured slippage the floor is a lower bound and the decision must say so")
}

// TestMaxPositionsCountsNewNamesOnly: topping up a name the book already holds
// does not widen it, so the cap must not refuse that.
func TestMaxPositionsCountsNewNamesOnly(t *testing.T) {
	l := risk.DefaultLimits()
	l.MaxPositions = 2
	g := gate(t, l, cost.Zerodha)

	book := risk.Book{
		Equity: 1_000_000,
		Positions: map[string]float64{
			strconv.FormatInt(id("A"), 10): 20_000, strconv.FormatInt(id("B"), 10): 20_000},
	}
	d, err := g.Check(testDay, book, []risk.Intent{buy("C", 20_000), buy("A", 20_000)})
	require.NoError(t, err)
	require.Len(t, d.Rejected, 1)
	require.Equal(t, "C", d.Rejected[0].Intent.Scrip)
	require.Equal(t, risk.RuleMaxPositions, d.Rejected[0].Rule)
	require.Len(t, d.Accepted, 1)
	require.Equal(t, "A", d.Accepted[0].Scrip)
}

// TestSingleStockAndBookCaps measures the post-trade position, not the intent,
// so a top-up that would breach the cap is refused even though the intent
// alone would not.
func TestSingleStockAndBookCaps(t *testing.T) {
	l := risk.DefaultLimits()
	l.MaxStockFraction = 0.10
	l.MaxBookNotional = 90_000
	g := gate(t, l, cost.Zerodha)

	book := risk.Book{Equity: 100_000,
		Positions: map[string]float64{strconv.FormatInt(id("A"), 10): 8_000}}

	// 8,000 held plus 8,000 more is 16% of a 100,000 book, over the 10% cap.
	d, err := g.Check(testDay, book, []risk.Intent{buy("A", 8_000)})
	require.NoError(t, err)
	require.Len(t, d.Rejected, 1)
	require.Equal(t, risk.RuleMaxStock, d.Rejected[0].Rule)

	// The book cap bites even when no single name does.
	wide := risk.Book{Equity: 10_000_000,
		Positions: map[string]float64{strconv.FormatInt(id("A"), 10): 85_000}}
	d2, err := g.Check(testDay, wide, []risk.Intent{buy("B", 10_000)})
	require.NoError(t, err)
	require.Len(t, d2.Rejected, 1)
	require.Equal(t, risk.RuleMaxBookNotional, d2.Rejected[0].Rule)
}

// TestDailyLossCapBlocksBuysAndNotSells: a loss cap that also blocked exits
// would be at its most dangerous exactly when it engaged.
func TestDailyLossCapBlocksBuysAndNotSells(t *testing.T) {
	l := risk.DefaultLimits()
	l.DailyLossCap = 5_000
	g := gate(t, l, cost.Zerodha)

	book := risk.Book{Equity: 200_000,
		Positions: map[string]float64{strconv.FormatInt(id("A"), 10): 50_000}, DayPnL: -5_000}
	d, err := g.Check(testDay, book, []risk.Intent{buy("B", 20_000), sell("A", 50_000)})
	require.NoError(t, err)
	require.Len(t, d.Accepted, 1)
	require.Equal(t, cost.Sell, d.Accepted[0].Side)
	require.Len(t, d.Rejected, 1)
	require.Equal(t, risk.RuleDailyLoss, d.Rejected[0].Rule)

	// One rupee short of the cap and the buy is fine, so the boundary is the
	// boundary and not an approximation of it.
	book.DayPnL = -4_999
	ok, err := g.Check(testDay, book, []risk.Intent{buy("B", 20_000)})
	require.NoError(t, err)
	require.Len(t, ok.Accepted, 1)
}

// TestBatchIsEvaluatedInOrder: accepted intents update the working book, so a
// batch that would breach a cap loses its LATER entries. The caller decides
// priority by ordering, and this pins that contract.
func TestBatchIsEvaluatedInOrder(t *testing.T) {
	l := risk.DefaultLimits()
	l.MaxBookNotional = 30_000
	g := gate(t, l, cost.Zerodha)
	book := risk.Book{Equity: 1_000_000, Positions: map[string]float64{}}

	d, err := g.Check(testDay, book, []risk.Intent{
		buy("FIRST", 20_000), buy("SECOND", 20_000),
	})
	require.NoError(t, err)
	require.Len(t, d.Accepted, 1)
	require.Equal(t, "FIRST", d.Accepted[0].Scrip)
	require.Equal(t, "SECOND", d.Rejected[0].Intent.Scrip)

	// Reversed, the other one survives: nothing about the pair decides it
	// except the order the caller chose.
	d2, err := g.Check(testDay, book, []risk.Intent{
		buy("SECOND", 20_000), buy("FIRST", 20_000),
	})
	require.NoError(t, err)
	require.Equal(t, "SECOND", d2.Accepted[0].Scrip)

	// And a rejected intent leaves no trace in the caller's book.
	require.Empty(t, book.Positions, "Check must not mutate the book it was handed")
}

// TestSlippageWidensTheFloor: the derived floor is a lower bound while
// slippage is unmodelled, and feeding a real number in has to move it.
func TestSlippageWidensTheFloor(t *testing.T) {
	s := sched(t, cost.Zerodha)
	bare, err := risk.MinNotional(s, testDay, cost.EquityDelivery, 0.005, 0)
	require.NoError(t, err)
	withSlip, err := risk.MinNotional(s, testDay, cost.EquityDelivery, 0.005, 10) // 10bps a leg
	require.NoError(t, err)
	require.Greater(t, withSlip, bare,
		"20bps of round-trip slippage must raise the floor, or the parameter is decorative")

	l := risk.DefaultLimits()
	l.SlippageBps = 10
	g := gate(t, l, cost.Zerodha)
	d, err := g.Check(testDay, risk.Book{Equity: 500_000}, nil)
	require.NoError(t, err)
	require.True(t, d.SlippageModelled)
	require.Equal(t, withSlip, d.MinNotional)
}

// TestMalformedIntentsAreRejectedNotPanicked: a zero-quantity intent is a
// strategy bug, and the gate is the last place it can be caught cheaply.
func TestMalformedIntentsAreRejectedNotPanicked(t *testing.T) {
	g := gate(t, risk.DefaultLimits(), cost.Zerodha)
	d, err := g.Check(testDay, risk.Book{Equity: 100_000}, []risk.Intent{
		{EntityID: 1, Scrip: "A", Side: cost.Buy, Product: cost.EquityDelivery, Quantity: 0, Price: 10},
		{EntityID: 2, Scrip: "B", Side: "hold", Product: cost.EquityDelivery, Quantity: 10, Price: 10},
	})
	require.NoError(t, err)
	require.Empty(t, d.Accepted)
	require.Len(t, d.RejectedBy(risk.RuleMalformed), 2)
}

// TestGateRefusesNonsenseLimits: a gate built on a bad config should fail at
// construction, not at the first rebalance six months into a backtest.
func TestGateRefusesNonsenseLimits(t *testing.T) {
	s := sched(t, cost.Zerodha)
	for _, tc := range []struct {
		name string
		l    risk.Limits
	}{
		{"no positions", risk.Limits{MaxPositions: 0, MaxRoundTripCost: 0.005, MaxStockFraction: 0.1}},
		{"no floor rule", risk.Limits{MaxPositions: 20, MaxStockFraction: 0.1}},
		{"stock fraction over one", risk.Limits{MaxPositions: 20, MaxRoundTripCost: 0.005, MaxStockFraction: 1.5}},
		{"negative slippage", risk.Limits{MaxPositions: 20, MaxRoundTripCost: 0.005, MaxStockFraction: 0.1, SlippageBps: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := risk.NewGate(tc.l, s)
			require.Error(t, err)
		})
	}
	_, err := risk.NewGate(risk.DefaultLimits(), nil)
	require.Error(t, err, "the floor cannot be derived without a schedule")
}
