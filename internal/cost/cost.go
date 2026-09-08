// Package cost prices an Indian equity trade the way a contract note does.
//
// Charges decided the sign in 38 of TradeLabs' 38 experiments, so this package
// is built against a real contract note rather than a rate card, and it keeps
// the two apart: every rate carries whether a document was actually seen to
// produce it, and a priced trade reports which of its components rested on a
// published rate nobody has reconciled. A backtest that used unverified rates
// is not refused -- that would leave the project unable to test anything --
// but it cannot come back looking like one that did not.
//
// Two things here came from the note and would not have come from anywhere
// else. GST is computed ONCE on the taxable value and rounded once; the
// CGST/SGST halves printed on a note are a display split, each rounded
// independently, and they sum to a paisa MORE than what was debited. And the
// DP charge is not on the contract note at all -- it is on the funds
// statement -- which is why it is levied here per scrip per SELL DAY through
// Day() and is structurally unreachable from Trade().
package cost

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Product is what is being traded, because the statutory rules differ by more
// than most rate cards admit. Buying units of an equity-oriented fund carries
// no STT at all, where buying a share carries 0.1%.
type Product string

const (
	EquityDelivery Product = "equity_delivery"
	FundUnits      Product = "fund_units" // ETFs and other equity-oriented fund units
)

// Side is buy or sell. Stamp duty is buy-only, the DP charge is sell-only, and
// STT differs by side for fund units.
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Component names one line on a contract note (or, for DP, one line on the
// funds statement that a contract note never shows).
type Component string

const (
	Brokerage    Component = "brokerage"
	STT          Component = "stt"
	ExchangeTxn  Component = "exchange_txn"
	SEBITurnover Component = "sebi_turnover"
	IPF          Component = "ipf"
	StampDuty    Component = "stamp_duty"
	GST          Component = "gst"
	DPCharge     Component = "dp_charge"
)

// Trade is one executed order. Scrip identifies the instrument for the purpose
// of the DP charge, which is levied once per scrip per sell day however many
// times that scrip was sold; any stable per-instrument key works, and the
// entity id is the one this project has.
type Trade struct {
	Date     time.Time
	Scrip    string
	Product  Product
	Side     Side
	Quantity int64
	Price    float64 // gross rate per unit, before brokerage
}

// Turnover is quantity times gross price: the base every ad valorem charge is
// struck on, and the value the note calls "Trade Value".
func (t Trade) Turnover() float64 { return float64(t.Quantity) * t.Price }

// Charges is one trade's costs, every line rounded to the paisa the way the
// note rounds it. DP is zero here and is added by Day.
type Charges struct {
	Turnover     float64
	Brokerage    float64
	STT          float64
	ExchangeTxn  float64
	SEBITurnover float64
	IPF          float64
	TaxableValue float64 // brokerage + exchange + SEBI + IPF; the note states this
	GST          float64
	StampDuty    float64
	DPCharge     float64

	// Unverified names every component whose rate came from a published rate
	// card that no contract note in this repository has reconciled. It is not
	// a warning to be logged and forgotten: the backtest report prints it, and
	// a result that leans on it is a result with a caveat attached.
	Unverified []Component
}

// Total is every charge. It is what turns a gross return into a net one.
func (c Charges) Total() float64 {
	return round2(c.Brokerage + c.STT + c.ExchangeTxn + c.SEBITurnover +
		c.IPF + c.GST + c.StampDuty + c.DPCharge)
}

// NetOutflow is what leaves the account on a buy; on a sell the proceeds are
// Turnover minus Total, so this is signed from the trader's point of view.
func (c Charges) NetOutflow() float64 { return round2(c.Turnover + c.Total()) }

// Trade prices one executed order, excluding the DP charge.
//
// The order of operations is the note's own and is not interchangeable: the
// ad valorem statutory lines are struck on turnover and rounded individually,
// the taxable value is their sum with brokerage, and GST is 18% of that sum
// rounded ONCE. Computing CGST and SGST separately at 9% and adding them
// produces 3.76 where the note debits 3.75.
func (s *Schedule) Trade(t Trade) (Charges, error) {
	if t.Quantity <= 0 {
		return Charges{}, fmt.Errorf("cost: quantity must be positive, got %d", t.Quantity)
	}
	if t.Price <= 0 {
		return Charges{}, fmt.Errorf("cost: price must be positive, got %g", t.Price)
	}
	if t.Side != Buy && t.Side != Sell {
		return Charges{}, fmt.Errorf("cost: unknown side %q", t.Side)
	}
	// Rounded to the paisa before anything is struck on it, because that is
	// what the note does: "Trade Value" is a rupee amount and every ad valorem
	// line is a percentage OF THE PRINTED FIGURE, not of an unrounded product.
	turnover := round2(t.Turnover())
	c := Charges{Turnover: turnover}
	var unverified []Component

	take := func(comp Component) (rate, error) {
		r, err := s.rateFor(comp, t.Product, t.Side, t.Date)
		if err != nil {
			return rate{}, err
		}
		if !r.Verified {
			unverified = append(unverified, comp)
		}
		return r, nil
	}

	br, err := take(Brokerage)
	if err != nil {
		return Charges{}, err
	}
	c.Brokerage = br.on(turnover)

	for _, comp := range []Component{ExchangeTxn, SEBITurnover, IPF} {
		r, err := take(comp)
		if err != nil {
			return Charges{}, err
		}
		switch comp {
		case ExchangeTxn:
			c.ExchangeTxn = r.on(turnover)
		case SEBITurnover:
			c.SEBITurnover = r.on(turnover)
		case IPF:
			c.IPF = r.on(turnover)
		}
	}

	// The note spells this sum out: "Taxable Value of Supply Includes Total
	// Brokerage + Exchange Transaction Charges + SEBI Turnover Fees + IPF
	// Charges". STT and stamp duty are taxes, not services, and stay out of it.
	c.TaxableValue = round2(c.Brokerage + c.ExchangeTxn + c.SEBITurnover + c.IPF)

	gst, err := take(GST)
	if err != nil {
		return Charges{}, err
	}
	c.GST = gst.on(c.TaxableValue)

	stt, err := take(STT)
	if err != nil {
		return Charges{}, err
	}
	c.STT = stt.on(turnover)

	stamp, err := take(StampDuty)
	if err != nil {
		return Charges{}, err
	}
	c.StampDuty = stamp.on(turnover)

	c.Unverified = dedupeComponents(unverified)
	return c, nil
}

// DayCharges is a whole session's costs for one account, with the DP charge
// applied the only way it is actually levied.
type DayCharges struct {
	Date       time.Time
	Trades     []Charges
	DPCharge   float64
	DPScrips   []string
	Unverified []Component
}

// Total is every trade's charges plus the day's DP.
func (d DayCharges) Total() float64 {
	var sum float64
	for _, c := range d.Trades {
		sum += c.Total()
	}
	return round2(sum + d.DPCharge)
}

// Day prices a session's trades and adds the DP charge once per distinct scrip
// SOLD, which is how the depository levies it: a flat rupee amount per scrip
// per day, independent of quantity and of how many sell orders it took.
//
// It exists as a separate entry point because there is no correct way to put
// the DP charge on a single trade. Charging it per trade double-counts a
// position sold in three clips; charging it on none of them under-reports by
// the one charge that dominates small orders, and the ~Rs10,000 minimum
// notional in RiskGate v1 is derived from this charge alone. Making Trade()
// unable to reach it is the point.
func (s *Schedule) Day(date time.Time, trades []Trade) (DayCharges, error) {
	out := DayCharges{Date: date}
	var unverified []Component
	sold := map[string]bool{}
	var product Product

	for _, t := range trades {
		if !sameDay(t.Date, date) {
			return DayCharges{}, fmt.Errorf(
				"cost: trade on %s passed to Day(%s); the DP charge is per scrip per DAY, so mixing sessions would undercount it",
				t.Date.Format(time.DateOnly), date.Format(time.DateOnly))
		}
		c, err := s.Trade(t)
		if err != nil {
			return DayCharges{}, err
		}
		out.Trades = append(out.Trades, c)
		unverified = append(unverified, c.Unverified...)
		if t.Side == Sell {
			sold[t.Scrip] = true
			product = t.Product
		}
	}

	if len(sold) > 0 {
		r, err := s.rateFor(DPCharge, product, Sell, date)
		if err != nil {
			return DayCharges{}, err
		}
		if !r.Verified {
			unverified = append(unverified, DPCharge)
		}
		for scrip := range sold {
			out.DPScrips = append(out.DPScrips, scrip)
		}
		sort.Strings(out.DPScrips)
		out.DPCharge = round2(float64(len(out.DPScrips)) * r.Value)
	}
	out.Unverified = dedupeComponents(unverified)
	return out, nil
}

// round2 rounds to the paisa, half away from zero, which is what reproduces
// the note: 1.8765 -> 1.88, 3.7530 -> 3.75, 0.02668 -> 0.03.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func dedupeComponents(in []Component) []Component {
	if len(in) == 0 {
		return nil
	}
	seen := map[Component]bool{}
	out := make([]Component, 0, len(in))
	for _, c := range in {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
