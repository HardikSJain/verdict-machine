package cost_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/cost"
)

// The golden contract note.
//
// Angel One, NSE cash, trade date 2026-09-07: BUY 98 units of Nippon India ETF
// Nifty BeES (INF204KB14I2) at 272.29, delivery, one order, one fill. Every
// figure below is off the note itself, and the whole point of the exercise is
// that the model must land on them to the paisa from the RATES rather than by
// storing the answers.
//
// The design puts this before internal/cost was written for a reason: charges
// decided the sign in 38 of TradeLabs' 38 experiments, and a cost model built
// from a rate card would have got the GST line wrong on every trade forever.
const (
	noteQty      = 98
	notePrice    = 272.29
	noteTurnover = 26684.42
	noteBrokerage,
	noteSTT,
	noteExchange,
	noteSEBI,
	noteIPF,
	noteTaxable,
	noteGST,
	noteStamp,
	noteTotal,
	noteNetPayable = 20.00, 0.00, 0.82, 0.03, 0.00, 20.85, 3.75, 4.00, 28.60, 26713.02
)

func noteTrade() cost.Trade {
	return cost.Trade{
		Date:     time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		Scrip:    "INF204KB14I2",
		Product:  cost.FundUnits,
		Side:     cost.Buy,
		Quantity: noteQty,
		Price:    notePrice,
	}
}

func TestGoldenContractNote(t *testing.T) {
	s, err := cost.NewSchedule(cost.AngelOne)
	require.NoError(t, err)

	c, err := s.Trade(noteTrade())
	require.NoError(t, err)

	for _, tc := range []struct {
		line string
		got  float64
		want float64
	}{
		{"trade value", c.Turnover, noteTurnover},
		{"brokerage", c.Brokerage, noteBrokerage},
		{"securities transaction tax", c.STT, noteSTT},
		{"exchange transaction charges", c.ExchangeTxn, noteExchange},
		{"SEBI turnover fees", c.SEBITurnover, noteSEBI},
		{"IPF charges", c.IPF, noteIPF},
		{"taxable value of supply", c.TaxableValue, noteTaxable},
		{"GST", c.GST, noteGST},
		{"stamp duty", c.StampDuty, noteStamp},
		{"total charges", c.Total(), noteTotal},
		{"net amount payable", c.NetOutflow(), noteNetPayable},
	} {
		require.InDelta(t, tc.want, tc.got, 1e-9, "contract note line %q", tc.line)
	}

	// STT is nil because this is a fund unit, not a share. The same trade in a
	// share would have carried 26.68, and getting that wrong by treating an
	// ETF like a stock is a 0.1% error on every leg.
	share := noteTrade()
	share.Product = cost.EquityDelivery
	sc, err := s.Trade(share)
	require.NoError(t, err)
	require.InDelta(t, 26.68, sc.STT, 0.005,
		"a share would have paid STT where the ETF paid none")

	// The note's own lines are verified; the stock STT above is not, and the
	// model has to keep saying so.
	require.Empty(t, c.Unverified,
		"every line on the golden note is backed by the note itself")
	require.Contains(t, sc.Unverified, cost.STT,
		"stock STT rests on a rate card until a stock contract note is reconciled")
}

// TestGSTIsRoundedOnceNotAsTwoHalves is the finding that justified the whole
// exercise, isolated so it cannot be lost in the golden test's noise.
//
// The note PRINTS CGST 1.88 and SGST 1.88. Add them and you get 3.76. The
// obligation total says 26,713.02, which only reconciles at 3.75. So GST is
// computed once at 18% of the taxable value and rounded once, and the halves
// are a display split with each half rounded independently.
//
// No rate card says this. A model built from published rates would compute
// 9% twice, be a paisa heavy on roughly half of all trades, and never fail a
// test that did not have a real document behind it.
func TestGSTIsRoundedOnceNotAsTwoHalves(t *testing.T) {
	s, err := cost.NewSchedule(cost.AngelOne)
	require.NoError(t, err)
	c, err := s.Trade(noteTrade())
	require.NoError(t, err)

	require.InDelta(t, 20.85, c.TaxableValue, 1e-9)

	half := round2(20.85 * 0.09)
	require.InDelta(t, 1.88, half, 1e-9, "each printed half rounds up to 1.88")
	require.InDelta(t, 3.76, half*2, 1e-9, "and the printed halves sum to 3.76")

	require.InDelta(t, 3.75, c.GST, 1e-9, "but 18% rounded once is 3.75")
	require.InDelta(t, noteNetPayable, c.NetOutflow(), 1e-9,
		"and only 3.75 reconciles the note's obligation total")

	wrong := c.NetOutflow() - c.GST + half*2
	require.InDelta(t, 26713.03, wrong, 1e-9,
		"the two-halves model overstates the note by exactly one paisa")
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// TestDPChargeIsPerScripPerSellDay pins the charge that is on no contract note
// and that the ~Rs10,000 minimum notional in RiskGate v1 is derived from.
//
// It is levied once per scrip per day however many sell orders it took, so a
// per-trade model double-counts a position sold in clips and a model that
// forgets it under-reports on exactly the small orders where it dominates.
func TestDPChargeIsPerScripPerSellDay(t *testing.T) {
	s, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

	sell := func(scrip string, qty int64) cost.Trade {
		return cost.Trade{Date: day, Scrip: scrip, Product: cost.EquityDelivery,
			Side: cost.Sell, Quantity: qty, Price: 500}
	}

	// One scrip, three clips: one DP charge.
	clipped, err := s.Day(day, []cost.Trade{sell("A", 10), sell("A", 10), sell("A", 10)})
	require.NoError(t, err)
	require.InDelta(t, 15.34, clipped.DPCharge, 1e-9)
	require.Equal(t, []string{"A"}, clipped.DPScrips)

	// Two scrips: two charges.
	two, err := s.Day(day, []cost.Trade{sell("A", 30), sell("B", 30)})
	require.NoError(t, err)
	require.InDelta(t, 30.68, two.DPCharge, 1e-9)
	require.Equal(t, []string{"A", "B"}, two.DPScrips)

	// Buys attract none.
	buys, err := s.Day(day, []cost.Trade{{Date: day, Scrip: "A", Product: cost.EquityDelivery,
		Side: cost.Buy, Quantity: 30, Price: 500}})
	require.NoError(t, err)
	require.Zero(t, buys.DPCharge)
	require.Empty(t, buys.DPScrips)

	// Trade() alone can never reach it, which is the structural half of this.
	one, err := s.Trade(sell("A", 30))
	require.NoError(t, err)
	require.Zero(t, one.DPCharge)
}

// TestDayRefusesTradesFromAnotherSession: the DP charge is per DAY, so letting
// two sessions into one Day() would levy one charge where two were due.
func TestDayRefusesTradesFromAnotherSession(t *testing.T) {
	s, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

	_, err = s.Day(day, []cost.Trade{{
		Date: day.AddDate(0, 0, 1), Scrip: "A", Product: cost.EquityDelivery,
		Side: cost.Sell, Quantity: 10, Price: 500,
	}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "per scrip per DAY")
}

// TestUnverifiedRatesAreReportedNotHidden is the honesty mechanism.
//
// A backtest is allowed to run on rates nobody has reconciled -- refusing
// would leave the project unable to test anything at all -- but it must not be
// able to come back looking like one that did not. These two dates use
// materially different tables and the difference has to be visible.
func TestUnverifiedRatesAreReportedNotHidden(t *testing.T) {
	s, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)

	old := cost.Trade{
		Date: time.Date(2015, 6, 30, 0, 0, 0, 0, time.UTC), Scrip: "X",
		Product: cost.EquityDelivery, Side: cost.Buy, Quantity: 100, Price: 500,
	}
	c, err := s.Trade(old)
	require.NoError(t, err)

	// 2015 predates uniform stamp duty (2020-07-01) and GST (2017-07-01)
	// entirely, so both lines are wrong by construction and say so.
	require.Contains(t, c.Unverified, cost.StampDuty)
	require.Contains(t, c.Unverified, cost.GST)
	require.Contains(t, c.Unverified, cost.STT)

	// And it still produces a number, because a backtest that cannot run
	// teaches nothing.
	require.Greater(t, c.Total(), 0.0)

	// The same trade shape on the note's own date leans on far less.
	recent := old
	recent.Date = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	rc, err := s.Trade(recent)
	require.NoError(t, err)
	require.Less(t, len(rc.Unverified), len(c.Unverified),
		"the recent date must rest on fewer unreconciled rates than the 2015 one")
}

// TestScheduleRefusesBeforeTheArchive: a date no entry claims to cover is an
// error, not a silent fallback to the nearest rate.
func TestScheduleRefusesBeforeTheArchive(t *testing.T) {
	s, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	_, err = s.Trade(cost.Trade{
		Date: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), Scrip: "X",
		Product: cost.EquityDelivery, Side: cost.Buy, Quantity: 10, Price: 100,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refuses to guess")
}

// TestRoundTripCostByBroker is the broker decision's arithmetic, computed
// rather than quoted, on the size RiskGate v1's minimum notional is set at.
//
// The gap is almost entirely brokerage and DP: two flat charges that do not
// scale, which is why the design fixes a minimum position size rather than a
// minimum percentage.
func TestRoundTripCostByBroker(t *testing.T) {
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	const qty, price = 40, 250.0 // Rs10,000, the RiskGate floor

	cost10k := func(b cost.Broker) float64 {
		s, err := cost.NewSchedule(b)
		require.NoError(t, err)
		buy, err := s.Day(day, []cost.Trade{{Date: day, Scrip: "X",
			Product: cost.EquityDelivery, Side: cost.Buy, Quantity: qty, Price: price}})
		require.NoError(t, err)
		sell, err := s.Day(day.AddDate(0, 1, 0), []cost.Trade{{Date: day.AddDate(0, 1, 0),
			Scrip: "X", Product: cost.EquityDelivery, Side: cost.Sell, Quantity: qty, Price: price}})
		require.NoError(t, err)
		return buy.Total() + sell.Total()
	}

	z, a := cost10k(cost.Zerodha), cost10k(cost.AngelOne)
	require.Less(t, z, a, "Zerodha is the cheaper venue for a delivery rebalance")

	// The design's rule is that round-trip cost stays under 0.5% of notional.
	const notional = qty * price
	require.Less(t, z/notional, 0.005,
		"Rs10,000 clears the 0.5% round-trip rule at Zerodha, which is where the floor came from")
	require.Greater(t, a/notional, 0.005,
		"and does not clear it at Angel One, so the floor is broker-dependent")
}

// TestRatesCarryASource: an entry with no source is an entry nobody can audit,
// and the whole schedule's claim to be evidence rests on every line naming
// where it came from.
func TestRatesCarryASource(t *testing.T) {
	s, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	rates := s.Rates()
	require.NotEmpty(t, rates)
	for _, r := range rates {
		require.NotEmpty(t, r.Describe(), "every rate must name its source")
		if !r.IsVerified() {
			// Either marker will do, and the second is the stronger claim:
			// UNVERIFIED means nobody has reconciled it, WRONG BY CONSTRUCTION
			// means we know the rate did not apply on those dates at all (GST
			// before 2017, national stamp duty before 2020).
			d := r.Describe()
			require.True(t,
				strings.Contains(d, "UNVERIFIED") || strings.Contains(d, "WRONG BY CONSTRUCTION"),
				"an unverified rate must say so in its source, not only in a flag: %s", d)
		}
	}
}
