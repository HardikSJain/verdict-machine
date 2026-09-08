package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/cost"
)

// Execution is what a session's opens did to a batch of orders.
type Execution struct {
	Fills    []Fill
	Unfilled []Unfilled
}

// Broker turns orders into fills. Paper is the backtest's implementation;
// Publisher (a Kite basket link the human confirms) and Kite arrive in M3 and
// phase 2, behind this same interface, so the loop above never learns which
// world it is in.
type Broker interface {
	Execute(ctx context.Context, date time.Time, orders []Order, opens map[int64]float64) (Execution, error)
}

// Paper fills at the session's open and charges the real cost model.
//
// It is not a stub. The whole question this project exists to answer is
// whether a rule survives costs, so the paper broker prices every fill through
// internal/cost with the same schedule the live broker will use, including the
// DP charge that no contract note shows.
type Paper struct {
	sched       *cost.Schedule
	slippageBps float64
}

// NewPaper returns a paper broker.
//
// slippageBps is charged against the fill on both sides: a buy pays the open
// plus it, a sell receives the open minus it. It defaults to zero at the
// caller's choice, and zero is honest rather than optimistic only because
// nothing has measured it yet -- the promotion gate in M4 wants slippage
// within band on at least 40 real fills, and that is the number that belongs
// here. Until then a backtest's fills are frictionless beyond the statutory
// charges, and the report has to say so.
func NewPaper(s *cost.Schedule, slippageBps float64) (*Paper, error) {
	if s == nil {
		return nil, fmt.Errorf("engine: paper broker needs a cost schedule")
	}
	if slippageBps < 0 {
		return nil, fmt.Errorf("engine: slippage must not be negative, got %g", slippageBps)
	}
	return &Paper{sched: s, slippageBps: slippageBps}, nil
}

// Execute fills what it can at the open.
//
// The whole batch goes through cost.Day rather than through cost.Trade one at
// a time, because the DP charge is levied once per scrip per SELL DAY. Pricing
// each order alone would charge it once per clip, which for a position exited
// in three orders is three times the real fee, on exactly the small positions
// where that fee decides whether the trade was worth doing.
func (p *Paper) Execute(ctx context.Context, date time.Time, orders []Order, opens map[int64]float64) (Execution, error) {
	var ex Execution
	var fillable []Order
	var trades []cost.Trade

	for _, o := range orders {
		open, ok := opens[o.EntityID]
		if !ok || open <= 0 {
			ex.Unfilled = append(ex.Unfilled, Unfilled{
				Order: o, Date: date,
				Reason: "no open price this session; the name did not trade",
			})
			continue
		}
		price := p.fillPrice(open, o.Side)
		fillable = append(fillable, o)
		trades = append(trades, cost.Trade{
			Date: date, Scrip: o.Scrip, Product: o.Product,
			Side: o.Side, Quantity: o.Quantity, Price: price,
		})
	}
	if len(fillable) == 0 {
		return ex, nil
	}

	day, err := p.sched.Day(date, trades)
	if err != nil {
		return Execution{}, fmt.Errorf("engine: pricing %s: %w", date.Format(time.DateOnly), err)
	}
	if len(day.Trades) != len(fillable) {
		return Execution{}, fmt.Errorf(
			"engine: cost model returned %d priced trades for %d orders", len(day.Trades), len(fillable))
	}

	// The day's DP charge belongs to the first sell of each scrip, because
	// that is the event that incurs it. Spreading it across a scrip's clips
	// would be equally correct in total and wrong in every per-trade figure a
	// report prints.
	var dpPerScrip float64
	if n := len(day.DPScrips); n > 0 {
		dpPerScrip = day.DPCharge / float64(n)
	}
	charged := map[string]bool{}

	for i, o := range fillable {
		c := day.Trades[i]
		if o.Side == cost.Sell && dpPerScrip > 0 && !charged[o.Scrip] {
			charged[o.Scrip] = true
			c.DPCharge = dpPerScrip
			c.Unverified = append(c.Unverified, cost.DPCharge)
		}
		ex.Fills = append(ex.Fills, Fill{
			Order: o, Date: date, Price: trades[i].Price, Charges: c,
		})
	}
	sort.Slice(ex.Fills, func(i, j int) bool { return ex.Fills[i].Scrip < ex.Fills[j].Scrip })
	return ex, nil
}

// fillPrice moves the open against the trader by the slippage assumption, in
// both directions. A model that applied it to one side only would flatter
// every round trip by half.
func (p *Paper) fillPrice(open float64, side cost.Side) float64 {
	if p.slippageBps == 0 {
		return open
	}
	adj := open * p.slippageBps / 10000
	if side == cost.Buy {
		return open + adj
	}
	return open - adj
}
