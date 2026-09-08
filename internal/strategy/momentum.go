// Package strategy holds the registered strategies. The first is 12-1
// cross-sectional momentum, which is the design's v1 and the hypothesis the
// first backtest is registered against.
package strategy

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/engine"
	"github.com/HardikSJain/verdict-machine/internal/risk"
)

// Momentum is 12-1 cross-sectional momentum: rank the liquid point-in-time
// universe by its return from twelve months ago to one month ago, hold the top
// N equal-weighted, rebalance monthly, and sit in cash whenever the market
// filter says the trend is down.
//
// The skipped final month is not a detail. Cross-sectional momentum reverses
// at the one-month horizon, so ranking on the most recent month buys what has
// just run and sells what has just fallen, which is a different and worse
// strategy wearing the same name.
//
// **The decision is made on the LAST session of a month and fills at the first
// open of the next**, which is what the design specifies and what the engine's
// next-open discipline enforces. The strategy never has to think about it: it
// emits intents on a close and the engine decides when they meet a price.
type Momentum struct {
	// FormationMonths is the far end of the ranking window (12).
	FormationMonths int
	// SkipMonths is the near end, excluded (1).
	SkipMonths int
	// Top is how many names to hold (20).
	Top int
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

	calendar  []time.Time
	rebalance map[string]bool
	journal   Journal
}

// NewMomentum returns the design's v1 parameters unless overridden.
func NewMomentum(top, formationMonths, skipMonths int, f MarketFilter) (*Momentum, error) {
	if top <= 0 {
		return nil, fmt.Errorf("strategy: top must be positive, got %d", top)
	}
	if formationMonths <= skipMonths || skipMonths < 0 {
		return nil, fmt.Errorf("strategy: formation %d must exceed skip %d and skip must not be negative",
			formationMonths, skipMonths)
	}
	if f == nil {
		return nil, fmt.Errorf("strategy: a market filter is required; pass AlwaysOn{} to disable it")
	}
	return &Momentum{
		FormationMonths: formationMonths, SkipMonths: skipMonths, Top: top, Filter: f,
		CashBuffer: 0.01,
		journal:    Journal{RefusedByScrip: map[string]int{}},
	}, nil
}

func (m *Momentum) Name() string {
	return fmt.Sprintf("momentum-%d-%d-top%d/%s",
		m.FormationMonths, m.SkipMonths, m.Top, m.Filter.Name())
}

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
	Liquidations    int
	ReturnsRefused  int
	ReturnsAbsent   int
	RefusedByScrip  map[string]int
	Rankings        []Ranking
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

	on, why, err := m.Filter.RiskOn(ctx, s)
	if err != nil {
		return nil, err
	}
	if !on {
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
				Scrip: h.Scrip, Side: cost.Sell, Product: cost.EquityDelivery,
				Quantity: h.Quantity, Price: px,
			})
		}
		_ = why
		return out, nil
	}

	if !m.rebalance[s.Date.Format(time.DateOnly)] {
		return nil, nil
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
		if sessions[i].Month() != sessions[i+1].Month() || sessions[i].Year() != sessions[i+1].Year() {
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
	ids := make([]int64, 0, len(universe))
	scrip := make(map[int64]string, len(universe))
	for _, u := range universe {
		ids = append(ids, u.EntityID)
		scrip[u.EntityID] = u.Scrip
	}

	from := s.Date.AddDate(0, -m.FormationMonths, 0)
	to := s.Date.AddDate(0, -m.SkipMonths, 0)
	rs, err := s.Market.Returns(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}

	m.journal.Rebalances++
	m.journal.ReturnsRefused += len(rs.Refused)
	m.journal.ReturnsAbsent += len(rs.Absent)
	for id := range rs.Refused {
		m.journal.RefusedByScrip[scrip[id]]++
	}

	type scored struct {
		id  int64
		ret float64
	}
	ranked := make([]scored, 0, len(rs.Priced))
	for id, r := range rs.Priced {
		ranked = append(ranked, scored{id, r})
	}
	// Descending by return; entity id breaks ties so two identical runs rank
	// identically. Ranging a map and hoping is how a backtest stops being
	// reproducible.
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].ret != ranked[j].ret {
			return ranked[i].ret > ranked[j].ret
		}
		return ranked[i].id < ranked[j].id
	})
	if len(ranked) > m.Top {
		ranked = ranked[:m.Top]
	}

	target := map[int64]int64{}
	perName := s.Equity * (1 - m.CashBuffer) / float64(m.Top)
	var selected []string
	for _, r := range ranked {
		px, ok := s.Closes[r.id]
		if !ok || px <= 0 {
			continue
		}
		qty := int64(perName / px)
		if qty <= 0 {
			continue
		}
		target[r.id] = qty
		selected = append(selected, scrip[r.id])
	}
	m.journal.Rankings = append(m.journal.Rankings, Ranking{
		Date: s.Date, Universe: len(universe), Priced: len(rs.Priced),
		Refused: len(rs.Refused), Absent: len(rs.Absent), Selected: selected,
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
		sells = append(sells, risk.Intent{
			Scrip: h.Scrip, Side: cost.Sell, Product: cost.EquityDelivery,
			Quantity: h.Quantity - want, Price: px,
		})
	}
	for _, r := range ranked {
		want, ok := target[r.id]
		if !ok {
			continue
		}
		have := s.Portfolio.Positions[r.id].Quantity
		if want <= have {
			continue
		}
		buys = append(buys, risk.Intent{
			Scrip: scrip[r.id], Side: cost.Buy, Product: cost.EquityDelivery,
			Quantity: want - have, Price: s.Closes[r.id],
		})
	}
	return append(sells, buys...), nil
}
