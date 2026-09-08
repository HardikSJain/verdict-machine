// Package engine steps a strategy over sessions and turns its intents into
// fills, with the risk gate and the cost model wired in rather than bolted on
// afterwards.
//
// The design caps this package's loop at roughly 500 lines and the reason is
// not tidiness. The same code has to run the backtest and the evening, because
// a backtest that exercises a different code path from the live run is
// measuring something nobody will ever trade. Everything that differs between
// those two worlds is behind a seam -- Clock, Market, Broker -- and the loop
// itself does not know which side of the seam it is on.
//
// **Signals are read at a close and orders fill at the NEXT open.** A strategy
// never trades at a price it has just seen. That one rule is the difference
// between a backtest and a fantasy, and it is enforced here rather than left
// to each strategy to remember.
package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/risk"
)

// Order is an intent that has passed the risk gate and is waiting for an open.
type Order struct {
	EntityID int64
	Scrip    string // ISIN or ticker, for the report and for the DP charge key
	Side     cost.Side
	Product  cost.Product
	Quantity int64

	// DecidedOn is the session whose close produced this order. It is carried
	// so a fill can be traced back to the information that caused it, which is
	// the whole of the next-open discipline.
	DecidedOn time.Time
}

// Fill is an executed order.
type Fill struct {
	Order
	Date    time.Time
	Price   float64
	Charges cost.Charges
}

// Value is the fill's turnover before charges.
func (f Fill) Value() float64 { return float64(f.Quantity) * f.Price }

// Unfilled is an order that reached an open and could not trade there, most
// often because the name did not trade that session. It is reported rather
// than dropped: an order that quietly evaporates is a backtest silently
// choosing not to take a trade it said it wanted.
type Unfilled struct {
	Order
	Date   time.Time
	Reason string
}

// Session is what a strategy sees: the state as of one session's CLOSE.
type Session struct {
	Date      time.Time
	Portfolio Portfolio
	Equity    float64
	Closes    map[int64]float64
	Market    Market
}

// Strategy turns a session into intents. It is handed the risk gate's floor
// but not the gate itself; sizing is the strategy's job and refusing is the
// gate's, and a strategy that could see the gate would start sizing to it.
type Strategy interface {
	Name() string
	Decide(ctx context.Context, s Session) ([]risk.Intent, error)
}

// Result is one backtest or one evening, with everything that happened.
type Result struct {
	Strategy   string
	From, To   time.Time
	Sessions   int
	Fills      []Fill
	Unfilled   []Unfilled
	Rejected   []risk.Rejection
	Equity     []EquityPoint
	Final      Portfolio
	FinalCash  float64
	TotalCosts float64

	// Unverified counts how many fills leaned on a charge rate that no
	// document in this repository has reconciled. The design requires the
	// report to print it beside the result, so it is a field and not a log
	// line.
	Unverified map[cost.Component]int

	// MinNotional is the floor that was in force, and SlippageModelled is
	// false while nothing has measured slippage, which makes that floor a
	// lower bound.
	MinNotional      float64
	SlippageModelled bool
}

// EquityPoint is the book marked at one session's close.
type EquityPoint struct {
	Date   time.Time
	Equity float64
	Cash   float64
	Held   int
}

// Engine is the loop.
type Engine struct {
	market   Market
	broker   Broker
	gate     *risk.Gate
	strategy Strategy
}

// New wires the seams together.
func New(m Market, b Broker, g *risk.Gate, s Strategy) (*Engine, error) {
	if m == nil || b == nil || g == nil || s == nil {
		return nil, fmt.Errorf("engine: market, broker, gate and strategy are all required")
	}
	return &Engine{market: m, broker: b, gate: g, strategy: s}, nil
}

// Run steps every session the clock supplies, in order.
//
// The order inside a session is the whole of the next-open discipline and is
// not interchangeable:
//
//  1. yesterday's orders meet today's OPEN and become fills
//  2. the fills move cash and positions
//  3. the book is marked at today's CLOSE
//  4. the strategy sees that close and decides
//  5. the gate accepts or refuses, and what survives waits for tomorrow's open
//
// A strategy therefore cannot act on a price in the same session it observed
// it, and orders decided on the final session never fill, which is correct:
// they were never given an open to trade at.
func (e *Engine) Run(ctx context.Context, cash float64) (Result, error) {
	sessions, err := e.market.Sessions(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(sessions) < 2 {
		return Result{}, fmt.Errorf("engine: need at least two sessions to fill anything, got %d", len(sessions))
	}

	p := NewPortfolio(cash)
	res := Result{
		Strategy:   e.strategy.Name(),
		From:       sessions[0],
		To:         sessions[len(sessions)-1],
		Sessions:   len(sessions),
		Unverified: map[cost.Component]int{},
	}
	var pending []Order

	// Derive the floor once up front so a run that never decided anything
	// still records the rule it was operating under. It is refreshed at every
	// decision below, because the charge schedule is dated and a 14-year
	// backtest genuinely crosses rate changes; what Result carries is the
	// floor in force at the LAST decision, not an average.
	if d, err := e.gate.Check(sessions[0], risk.Book{}, nil); err == nil {
		res.MinNotional = d.MinNotional
		res.SlippageModelled = d.SlippageModelled
	} else {
		return Result{}, err
	}

	for _, date := range sessions {
		bars, err := e.market.Bars(ctx, date)
		if err != nil {
			return Result{}, fmt.Errorf("engine: bars for %s: %w", date.Format(time.DateOnly), err)
		}

		// 1 and 2: yesterday's orders meet today's open.
		if len(pending) > 0 {
			opens := make(map[int64]float64, len(bars))
			for id, b := range bars {
				opens[id] = b.Open
			}
			ex, err := e.broker.Execute(ctx, date, pending, opens)
			if err != nil {
				return Result{}, err
			}
			for _, f := range ex.Fills {
				if err := p.Apply(f); err != nil {
					return Result{}, fmt.Errorf("engine: %s: %w", date.Format(time.DateOnly), err)
				}
				res.TotalCosts += f.Charges.Total()
				for _, c := range f.Charges.Unverified {
					res.Unverified[c]++
				}
			}
			res.Fills = append(res.Fills, ex.Fills...)
			res.Unfilled = append(res.Unfilled, ex.Unfilled...)
			pending = nil
		}

		// 3: mark at the close.
		closes := make(map[int64]float64, len(bars))
		for id, b := range bars {
			closes[id] = b.Close
		}
		p.Mark(closes)
		equity := p.Equity(closes)
		res.Equity = append(res.Equity, EquityPoint{
			Date: date, Equity: equity, Cash: p.Cash, Held: len(p.Positions),
		})

		// 4: the strategy sees the close.
		intents, err := e.strategy.Decide(ctx, Session{
			Date: date, Portfolio: p.Clone(), Equity: equity, Closes: closes, Market: e.market,
		})
		if err != nil {
			return Result{}, fmt.Errorf("engine: %s on %s: %w", e.strategy.Name(), date.Format(time.DateOnly), err)
		}
		if len(intents) == 0 {
			continue
		}

		// 5: the gate.
		d, err := e.gate.Check(date, e.book(p, closes, equity), intents)
		if err != nil {
			return Result{}, err
		}
		res.Rejected = append(res.Rejected, d.Rejected...)
		res.MinNotional = d.MinNotional
		res.SlippageModelled = d.SlippageModelled

		for _, in := range d.Accepted {
			id, ok := e.entityOf(in, bars)
			if !ok {
				res.Unfilled = append(res.Unfilled, Unfilled{
					Order:  Order{Scrip: in.Scrip, Side: in.Side, Product: in.Product, Quantity: in.Quantity, DecidedOn: date},
					Date:   date,
					Reason: "accepted intent names a scrip with no bar on the deciding session",
				})
				continue
			}
			pending = append(pending, Order{
				EntityID: id, Scrip: in.Scrip, Side: in.Side,
				Product: in.Product, Quantity: in.Quantity, DecidedOn: date,
			})
		}
	}

	res.Final = p.Clone()
	res.FinalCash = p.Cash
	sort.Slice(res.Fills, func(i, j int) bool {
		if !res.Fills[i].Date.Equal(res.Fills[j].Date) {
			return res.Fills[i].Date.Before(res.Fills[j].Date)
		}
		return res.Fills[i].Scrip < res.Fills[j].Scrip
	})
	return res, nil
}

// book turns the portfolio into what the risk gate reads. Notionals are marked
// at the close, so a cap is measured against what the position is worth now
// and not against what it cost.
func (e *Engine) book(p *Portfolio, closes map[int64]float64, equity float64) risk.Book {
	pos := make(map[string]float64, len(p.Positions))
	for id, held := range p.Positions {
		mark, ok := closes[id]
		if !ok {
			mark = held.LastMark
		}
		pos[held.Scrip] = float64(held.Quantity) * mark
		_ = id
	}
	return risk.Book{Equity: equity, Positions: pos}
}

// entityOf resolves an intent's scrip back to the entity holding a bar on the
// deciding session. The strategy names scrips because that is what a human and
// a broker both read; the engine needs the id to price a fill.
func (e *Engine) entityOf(in risk.Intent, bars map[int64]Bar) (int64, bool) {
	for id, b := range bars {
		if b.Scrip == in.Scrip {
			return id, true
		}
	}
	return 0, false
}
