// Package strategy holds the registered strategies. The first is 12-1
// cross-sectional momentum, which is the design's v1 and the hypothesis the
// first backtest is registered against.
package strategy

import (
	"context"
	"fmt"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/engine"
	"github.com/HardikSJain/verdict-machine/internal/risk"
)

// Selector is the shared harness for every rule in docs/experiments: rank the
// point-in-time universe with a Ranker, hold the top N equal-weighted, rebalance
// on a cadence, and sit in cash whenever the market filter says the trend is
// down.
//
// Everything except the ranking is deliberately common. Two rules compared on
// different cadences, different sizing or a different fill assumption are not
// being compared at all, and the easiest way to accidentally produce a winner is
// to give it a harness the others did not have.
//
// **The decision is made on the LAST session of a rebalance period and fills at
// the first open of the next**, which the engine's next-open discipline
// enforces. The strategy never has to think about it: it emits intents on a
// close and the engine decides when they meet a price.
type Selector struct {
	// Ranker decides the order. It is the only thing that differs between the
	// registered strategies.
	Ranker Ranker
	// Top is how many names to hold.
	Top int
	// RebalanceMonths is the cadence: 1 is monthly, 12 is annual. Cost is the
	// binding constraint on every rule tested here -- experiment 002 paid 6.7%
	// of capital at 7.4x turnover -- so the cadence is a first-class parameter
	// rather than an assumption baked into the loop.
	RebalanceMonths int
	// Filter decides risk-on or risk-off. Never nil; use AlwaysOn to disable.
	Filter MarketFilter

	// CashBuffer is the fraction of equity NOT allocated, and it exists for a
	// structural reason rather than a cautious one: sizing happens on a CLOSE
	// and filling happens at the next OPEN, so a book sized to exactly its own
	// equity is unaffordable by any morning the market gaps up. Charges take
	// another ~0.12% of a buy on top.
	//
	// It is an ASSUMPTION, not a measurement. 1% covers the charges and a
	// typical Indian large-cap overnight move with room to spare, but the
	// honest version of this number is the observed distribution of gaps
	// between a rebalance close and the next open, which nothing has computed
	// yet. Too small and the marginal names go unfilled (visibly, in
	// Result.Unfilled); too large and the book carries permanent cash drag.
	CashBuffer float64

	// MinTradeNotional is the smallest rupee delta worth trading. Below it a
	// position is left to drift rather than adjusted.
	//
	// It exists because omitting it produced a silent, compounding cash leak.
	// A rebalance computes a target per name and trades the difference. The
	// SELL side of that difference always executes -- reducing an overweight
	// name raises real cash -- while the BUY side is a small top-up that the
	// risk gate refuses for being below its minimum notional. One side of the
	// rebalance completes and the other does not, so cash accumulates every
	// period and never returns to the market. Measured on the equal-weight
	// yardstick over 2013-2026, the book sat on 15-20% cash permanently and
	// 152 of its buys were refused on notional alone.
	//
	// A band fixes it by making the rule symmetric: a name whose drift is too
	// small to be worth buying is also too small to be worth selling, so the
	// cash is never raised in the first place. That is also what a real manager
	// does, and it is why rebalance bands exist outside backtests.
	MinTradeNotional float64

	// RebalanceOnce buys the first selection and never trades again.
	//
	// It is a diagnostic rather than a strategy: comparing it against the same
	// rule rebalanced on a cadence separates a signal's contribution from the
	// mechanical effect of rebalancing itself. Equal-weighting a universe and
	// restoring those weights every year means buying more of whatever fell,
	// which in a turnover-ranked Indian universe holding eventual zeros is a
	// value trap rather than a discipline. This is how to measure that.
	RebalanceOnce  bool
	rebalancedOnce bool

	label     string
	calendar  []time.Time
	rebalance map[string]bool
	journal   Journal
}

// Momentum is retained as the name experiments 001 and 002 were run under.
type Momentum = Selector

// New builds a Selector.
func New(label string, r Ranker, top, rebalanceMonths int, f MarketFilter) (*Selector, error) {
	if r == nil {
		return nil, fmt.Errorf("strategy: a ranker is required")
	}
	if top <= 0 {
		return nil, fmt.Errorf("strategy: top must be positive, got %d", top)
	}
	if rebalanceMonths <= 0 || 12%rebalanceMonths != 0 {
		return nil, fmt.Errorf(
			"strategy: rebalance months must divide 12 (1, 2, 3, 4, 6 or 12), got %d", rebalanceMonths)
	}
	if f == nil {
		return nil, fmt.Errorf("strategy: a market filter is required; pass AlwaysOn{} to disable it")
	}
	return &Selector{
		Ranker: r, Top: top, RebalanceMonths: rebalanceMonths, Filter: f,
		CashBuffer: 0.01, label: label,
		journal: Journal{RefusedByScrip: map[string]int{}},
	}, nil
}

// NewMomentum is experiment 001 and 002's rule, kept so those results stay
// reproducible from the same call.
func NewMomentum(top, formationMonths, skipMonths int, f MarketFilter) (*Momentum, error) {
	if formationMonths <= skipMonths || skipMonths < 0 {
		return nil, fmt.Errorf("strategy: formation %d must exceed skip %d and skip must not be negative",
			formationMonths, skipMonths)
	}
	label := fmt.Sprintf("momentum-%d-%d-top%d", formationMonths, skipMonths, top)
	return New(label, ReturnRanker{
		FormationMonths: formationMonths, SkipMonths: skipMonths, Label: label,
	}, top, 1, f)
}

func (m *Selector) Name() string { return m.label + "/" + m.Filter.Name() }

// Journal reports what the strategy saw but could not act on. The engine's
// Result carries fills, unfilled orders and risk rejections; this carries the
// things that never became intents at all -- names the succession fence
// refused, names with no price at an endpoint, and the sessions the filter sat
// out. Without it a backtest silently ranks a smaller universe than it claims.
func (m *Momentum) Journal() Journal { return m.journal }

// Journal is the strategy's own record.
type Journal struct {
	Rebalances      int
	RiskOffSessions int
	// UnknownSessions counts sessions the filter could not read at all --
	// warm-up, or a session missing from the index archive. They are counted
	// apart from RiskOffSessions because sitting out on a signal and sitting
	// out on a gap are different facts about a backtest.
	// DriftSkipped counts adjustments left untraded because they fell inside
	// the rebalance band. They are a deliberate choice, not a failure, and the
	// count says how often the book was allowed to drift.
	DriftSkipped      int
	UnknownSessions   int
	LastUnknownReason string
	Liquidations      int
	ReturnsRefused    int
	ReturnsAbsent     int
	RefusedByScrip    map[string]int
	Rankings          []Ranking
}

// Ranking is one rebalance's view, kept so a report can show what was ranked
// and how much of the universe was unusable that month.
type Ranking struct {
	Date     time.Time
	Universe int
	Priced   int
	Refused  int
	Absent   int
	Selected []string
}

func (m *Momentum) Decide(ctx context.Context, s engine.Session) ([]risk.Intent, error) {
	if err := m.loadCalendar(ctx, s); err != nil {
		return nil, err
	}

	stance, why, err := m.Filter.Stance(ctx, s)
	if err != nil {
		return nil, err
	}
	if stance == Unknown {
		// The filter cannot see. Hold what is held, open nothing, and record
		// it: a session sat out for want of data must not look like a session
		// sat out on a signal.
		m.journal.UnknownSessions++
		m.journal.LastUnknownReason = why
		return nil, nil
	}
	if stance == RiskOff {
		m.journal.RiskOffSessions++
		// Liquidate whatever is held, on any session, not only a rebalance
		// one. The design says the book goes to cash when the trend breaks and
		// waits there, and a filter that only acted at month end would hold
		// through most of a drawdown it had already flagged.
		held := s.Portfolio.Held()
		if len(held) == 0 {
			return nil, nil
		}
		m.journal.Liquidations++
		out := make([]risk.Intent, 0, len(held))
		for _, h := range held {
			px, ok := s.Closes[h.EntityID]
			if !ok || px <= 0 {
				px = h.LastMark
			}
			out = append(out, risk.Intent{
				EntityID: h.EntityID, Scrip: h.Scrip, Side: cost.Sell,
				Product: cost.EquityDelivery, Quantity: h.Quantity, Price: px,
			})
		}
		_ = why
		return out, nil
	}

	if !m.rebalance[s.Date.Format(time.DateOnly)] {
		return nil, nil
	}
	if m.RebalanceOnce {
		if m.rebalancedOnce {
			return nil, nil
		}
		m.rebalancedOnce = true
	}
	return m.rebalanceTo(ctx, s)
}

// loadCalendar reads the session list once and marks the last session of each
// month.
//
// "First trading day of the month" cannot be computed from a calendar date --
// the first of January is a holiday, and NSE trades some Saturdays -- so it is
// read off the sessions the archive actually holds, the same authority
// backfill uses. A month-end rule built on weekday arithmetic is how M0 lost
// 19 real sessions.
func (m *Momentum) loadCalendar(ctx context.Context, s engine.Session) error {
	if m.calendar != nil {
		return nil
	}
	sessions, err := s.Market.Sessions(ctx)
	if err != nil {
		return err
	}
	m.calendar = sessions
	m.rebalance = make(map[string]bool, 200)
	for i := 0; i < len(sessions)-1; i++ {
		if sessions[i].Month() == sessions[i+1].Month() && sessions[i].Year() == sessions[i+1].Year() {
			continue
		}
		// The last session of a month. Whether it is also a rebalance depends
		// on the cadence: monthly takes every one, annual takes December's.
		if int(sessions[i].Month())%m.RebalanceMonths == 0 {
			m.rebalance[sessions[i].Format(time.DateOnly)] = true
		}
	}
	return nil
}

func (m *Momentum) rebalanceTo(ctx context.Context, s engine.Session) ([]risk.Intent, error) {
	universe, err := s.Market.Universe(ctx, s.Date)
	if err != nil {
		return nil, err
	}
	if len(universe) == 0 {
		return nil, nil
	}
	scrip := make(map[int64]string, len(universe))
	for _, u := range universe {
		scrip[u.EntityID] = u.Scrip
	}

	order, refused, absent, err := m.Ranker.Rank(ctx, s, universe)
	if err != nil {
		return nil, err
	}

	m.journal.Rebalances++
	m.journal.ReturnsRefused += len(refused)
	m.journal.ReturnsAbsent += len(absent)
	for id := range refused {
		m.journal.RefusedByScrip[scrip[id]]++
	}

	priced := len(order)
	ranked := order
	if len(ranked) > m.Top {
		ranked = ranked[:m.Top]
	}

	target := map[int64]int64{}
	perName := s.Equity * (1 - m.CashBuffer) / float64(m.Top)
	var selected []string
	for _, id := range ranked {
		px, ok := s.Closes[id]
		if !ok || px <= 0 {
			continue
		}
		qty := int64(perName / px)
		if qty <= 0 {
			continue
		}
		target[id] = qty
		selected = append(selected, scrip[id])
	}
	m.journal.Rankings = append(m.journal.Rankings, Ranking{
		Date: s.Date, Universe: len(universe), Priced: priced,
		Refused: len(refused), Absent: len(absent), Selected: selected,
	})

	// Sells first, then buys. The engine applies them in that order anyway,
	// but the risk gate walks a batch in the order it is given and updates its
	// working book as it goes, so a batch that led with buys would be refused
	// against a book that had not yet released the names it was leaving.
	var sells, buys []risk.Intent
	held := s.Portfolio.Held()
	for _, h := range held {
		px, ok := s.Closes[h.EntityID]
		if !ok || px <= 0 {
			px = h.LastMark
		}
		want := target[h.EntityID]
		if want >= h.Quantity {
			continue
		}
		qty := h.Quantity - want
		// A full exit always trades: a name that has left the target set has to
		// go regardless of how small the remaining stub is, or it stays in the
		// book forever. Only a partial trim is subject to the band.
		if want > 0 && float64(qty)*px < m.MinTradeNotional {
			m.journal.DriftSkipped++
			continue
		}
		sells = append(sells, risk.Intent{
			EntityID: h.EntityID, Scrip: h.Scrip, Side: cost.Sell,
			Product: cost.EquityDelivery, Quantity: qty, Price: px,
		})
	}
	for _, id := range ranked {
		want, ok := target[id]
		if !ok {
			continue
		}
		have := s.Portfolio.Positions[id].Quantity
		if want <= have {
			continue
		}
		qty := want - have
		// The same band on the buy side. A new position always trades; a
		// top-up of an existing one must clear the band, or the gate would
		// refuse it and the cash raised to fund it would sit idle.
		if have > 0 && float64(qty)*s.Closes[id] < m.MinTradeNotional {
			m.journal.DriftSkipped++
			continue
		}
		buys = append(buys, risk.Intent{
			EntityID: id, Scrip: scrip[id], Side: cost.Buy,
			Product: cost.EquityDelivery, Quantity: qty, Price: s.Closes[id],
		})
	}
	return append(sells, buys...), nil
}
