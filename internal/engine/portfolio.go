package engine

import (
	"fmt"
	"sort"

	"github.com/HardikSJain/verdict-machine/internal/cost"
)

// Position is one holding. CostBasis is what was paid for the shares still
// held, charges included, so a realised P&L is a subtraction rather than a
// reconstruction.
type Position struct {
	EntityID  int64
	Scrip     string
	Quantity  int64
	CostBasis float64

	// LastMark is the most recent price this position was valued at, used when
	// a session has no bar for it -- a suspension, a halt -- so that the book
	// does not silently value a held name at zero.
	LastMark float64
}

// Portfolio is cash plus positions. It is a read model in the design's sense:
// the ledger will rebuild it from events in M2, and nothing here is a source
// of truth once that exists.
type Portfolio struct {
	Cash      float64
	Positions map[int64]Position
}

// NewPortfolio starts with cash and nothing held.
func NewPortfolio(cash float64) *Portfolio {
	return &Portfolio{Cash: cash, Positions: map[int64]Position{}}
}

// Clone is a deep copy, so a strategy handed the portfolio cannot reach into
// the engine's own state.
func (p *Portfolio) Clone() Portfolio {
	out := Portfolio{Cash: p.Cash, Positions: make(map[int64]Position, len(p.Positions))}
	for k, v := range p.Positions {
		out.Positions[k] = v
	}
	return out
}

// Apply moves cash and shares for one fill.
//
// Charges are paid out of cash on BOTH sides: a buy costs turnover plus
// charges, a sell returns turnover minus charges. Netting them into the price
// instead would make the position's cost basis a fiction and the realised P&L
// wrong by the charges, which for a strategy whose whole question is "does it
// survive costs" is the one error that must not be possible.
func (p *Portfolio) Apply(f Fill) error {
	if f.Quantity <= 0 {
		return fmt.Errorf("fill for %s has quantity %d", f.Scrip, f.Quantity)
	}
	value := f.Value()
	charges := f.Charges.Total()
	held := p.Positions[f.EntityID]

	switch f.Side {
	case cost.Buy:
		outflow := value + charges
		if outflow > p.Cash+1e-9 {
			return fmt.Errorf(
				"buy %d %s at %.2f needs %.2f including %.2f of charges, and the book holds %.2f",
				f.Quantity, f.Scrip, f.Price, outflow, charges, p.Cash)
		}
		p.Cash -= outflow
		held.EntityID = f.EntityID
		held.Scrip = f.Scrip
		held.Quantity += f.Quantity
		held.CostBasis += outflow
		held.LastMark = f.Price
		p.Positions[f.EntityID] = held

	case cost.Sell:
		if held.Quantity < f.Quantity {
			return fmt.Errorf("sell %d %s but the book holds %d", f.Quantity, f.Scrip, held.Quantity)
		}
		p.Cash += value - charges
		// Average cost: the basis leaves in proportion to the shares.
		basisOut := held.CostBasis * float64(f.Quantity) / float64(held.Quantity)
		held.Quantity -= f.Quantity
		held.CostBasis -= basisOut
		held.LastMark = f.Price
		if held.Quantity == 0 {
			delete(p.Positions, f.EntityID)
		} else {
			p.Positions[f.EntityID] = held
		}

	default:
		return fmt.Errorf("fill for %s has unknown side %q", f.Scrip, f.Side)
	}
	return nil
}

// Mark records this session's closes against the positions that have one.
//
// It exists because LastMark used to be written only by Apply, which meant it
// held the FILL price forever: a name bought at 100, trading at 120 for six
// months, then halted for a session, would value at 100 again for that session
// and print a 17% drawdown and a 17% recovery around a day on which nothing
// happened. The engine calls this at every close, so LastMark is the most
// recent price the position was actually seen at.
func (p *Portfolio) Mark(closes map[int64]float64) {
	for id, held := range p.Positions {
		if c, ok := closes[id]; ok && c > 0 {
			held.LastMark = c
			p.Positions[id] = held
		}
	}
}

// Equity is cash plus every position marked at the given closes. A position
// with no close this session keeps its LastMark rather than vanishing, and a
// position that has never been marked at all values at its cost basis.
func (p *Portfolio) Equity(closes map[int64]float64) float64 {
	total := p.Cash
	for id, held := range p.Positions {
		mark, ok := closes[id]
		if !ok {
			mark = held.LastMark
		}
		if mark <= 0 {
			total += held.CostBasis
			continue
		}
		total += float64(held.Quantity) * mark
	}
	return total
}

// Held returns the positions sorted by scrip, for a report that needs a stable
// order. Ranging a map would make two identical runs print differently.
func (p *Portfolio) Held() []Position {
	out := make([]Position, 0, len(p.Positions))
	for _, v := range p.Positions {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scrip < out[j].Scrip })
	return out
}
