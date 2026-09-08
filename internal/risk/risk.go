// Package risk is RiskGate v1: the rules that stand between a strategy's
// intent and an order.
//
// Two principles decide everything here, and both are choices worth arguing
// with rather than conventions.
//
// **A limit may block an increase in exposure. It may never block a
// reduction.** Every cap below applies to buys only. A gate that can refuse a
// sell is a gate that can trap the book in a position it has decided it wants
// out of, and the cost of that failure is unbounded where the cost of an
// oversized position is not. The one exception is the kill switch, which stops
// the machine entirely on the reasoning that a human with a Kite login can
// always sell by hand -- see KillSwitch.
//
// **The minimum position size is derived, not configured.** The design fixed
// it at "roughly Rs10k at current charges", and now that internal/cost can
// price a round trip the floor is solved for instead: the smallest notional
// whose modelled round-trip cost stays under MaxRoundTripCost. That makes it
// automatically correct per broker and per date, and it was worth doing
// because the answer is broker-dependent -- Rs10,000 clears 0.5% at Zerodha
// (0.376%) and does not at Angel One (0.695%).
package risk

import (
	"fmt"
	"sort"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/cost"
)

// Limits are the RiskGate v1 parameters. They are JSON-tagged because the
// design folds them into `config_hash`; the hashing itself belongs in
// cmd/verdict with the rest of config loading and lands with `runs`.
type Limits struct {
	// MaxPositions caps how many distinct names the book may hold. The DP
	// charge is flat per scrip per sell day, so a wide book inside a small
	// play tier is single-share lots paying more in depository fees than they
	// can earn.
	MaxPositions int `json:"max_positions"`

	// MaxRoundTripCost is the fraction of notional a modelled round trip may
	// cost. MinNotional is solved from it.
	MaxRoundTripCost float64 `json:"max_round_trip_cost"`

	// MinNotional overrides the derived floor when non-zero. Leave it at zero
	// in production: a hardcoded floor silently stops being right the day a
	// charge changes, which is the failure this package was rewritten to
	// avoid.
	MinNotional float64 `json:"min_notional"`

	// MaxStockFraction caps one name as a fraction of algo-book equity.
	MaxStockFraction float64 `json:"max_stock_fraction"`

	// MaxBookNotional caps the whole algo book in rupees. It is the play tier,
	// and the design's constraint that the algo never touches the core SIP is
	// enforced here or nowhere.
	MaxBookNotional float64 `json:"max_book_notional"`

	// DailyLossCap is a positive rupee amount. Once the day's P&L is at or
	// below its negative, no further buys are accepted.
	DailyLossCap float64 `json:"daily_loss_cap"`

	// SlippageBps is added to both legs when solving for MinNotional.
	//
	// It is ZERO by default and that is not an oversight: no slippage has been
	// measured yet, and inventing a number would put a fabricated constant
	// inside a floor the whole book is sized against. Until the alert phase
	// measures real fills -- the promotion gate wants slippage within band on
	// at least 40 of them -- the derived floor is a LOWER bound, and
	// Decision.SlippageModelled says so on every run.
	SlippageBps float64 `json:"slippage_bps"`
}

// DefaultLimits is the design's RiskGate v1, with the floor left to be
// derived.
func DefaultLimits() Limits {
	return Limits{
		MaxPositions:     20,
		MaxRoundTripCost: 0.005,
		MaxStockFraction: 0.10,
		DailyLossCap:     0,
		SlippageBps:      0,
	}
}

func (l Limits) validate() error {
	switch {
	case l.MaxPositions <= 0:
		return fmt.Errorf("risk: max_positions must be positive, got %d", l.MaxPositions)
	case l.MaxRoundTripCost <= 0 && l.MinNotional <= 0:
		return fmt.Errorf("risk: set max_round_trip_cost so the floor can be derived, or min_notional to override it")
	case l.MaxStockFraction <= 0 || l.MaxStockFraction > 1:
		return fmt.Errorf("risk: max_stock_fraction must be in (0, 1], got %g", l.MaxStockFraction)
	case l.SlippageBps < 0:
		return fmt.Errorf("risk: slippage_bps must not be negative, got %g", l.SlippageBps)
	}
	return nil
}

// Intent is one proposed order.
type Intent struct {
	Scrip    string
	Side     cost.Side
	Product  cost.Product
	Quantity int64
	Price    float64
}

// Notional is what the intent would put to work.
func (i Intent) Notional() float64 { return float64(i.Quantity) * i.Price }

// Book is the state the gate reads before deciding. Positions are current
// notionals by scrip.
type Book struct {
	Equity    float64
	Positions map[string]float64

	// DayPnL is realised plus unrealised for the session, negative for a loss.
	DayPnL float64

	// KillSwitch is empty when disengaged and carries the reason when not.
	// The design stores it as a row so the binary stays stateless; reading
	// that row lands with the ledger in M2, and until then the caller supplies
	// it.
	KillSwitch string
}

func (b Book) positionOf(scrip string) float64 { return b.Positions[scrip] }

// Rejection is one refused intent and the rule that refused it. The design
// requires these to become `risk.rejected` events rather than silent drops,
// and in particular requires that an intent below the floor is REJECTED and
// not rounded up: rounding up would size a position off a risk rule instead of
// off the strategy.
type Rejection struct {
	Intent Intent
	Rule   string
	Detail string
}

func (r Rejection) String() string {
	return fmt.Sprintf("%s %s %d @ %.2f rejected by %s: %s",
		r.Intent.Side, r.Intent.Scrip, r.Intent.Quantity, r.Intent.Price, r.Rule, r.Detail)
}

// Rule names, stable enough to key an event payload on.
const (
	RuleKillSwitch      = "kill_switch"
	RuleDailyLoss       = "daily_loss_cap"
	RuleMinNotional     = "min_notional"
	RuleMaxPositions    = "max_positions"
	RuleMaxStock        = "max_stock_fraction"
	RuleMaxBookNotional = "max_book_notional"
	RuleMalformed       = "malformed_intent"
)

// Decision is the gate's verdict on a batch.
type Decision struct {
	Accepted []Intent
	Rejected []Rejection

	// MinNotional is the floor that was in force, whether configured or
	// derived, so a run can record which number it actually used.
	MinNotional float64

	// SlippageModelled is false while Limits.SlippageBps is zero, which makes
	// MinNotional a lower bound rather than the real floor.
	SlippageModelled bool
}

// Gate applies Limits against a Book.
type Gate struct {
	limits Limits
	sched  *cost.Schedule
}

// NewGate returns a gate. The schedule is required even when MinNotional is
// configured, because the derivation is what a report prints beside an
// overridden floor.
func NewGate(l Limits, s *cost.Schedule) (*Gate, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("risk: a cost schedule is required to derive the position floor")
	}
	return &Gate{limits: l, sched: s}, nil
}

// Limits returns the parameters in force.
func (g *Gate) Limits() Limits { return g.limits }

// Check decides a batch of intents.
//
// Intents are evaluated IN THE ORDER GIVEN and each accepted one updates the
// working book, so a batch that would breach a cap has its later entries
// rejected rather than its earlier ones. The caller therefore decides
// priority; a strategy that cares should pass its intents strongest first.
func (g *Gate) Check(date time.Time, b Book, intents []Intent) (Decision, error) {
	floor := g.limits.MinNotional
	if floor <= 0 {
		derived, err := MinNotional(g.sched, date, cost.EquityDelivery,
			g.limits.MaxRoundTripCost, g.limits.SlippageBps)
		if err != nil {
			return Decision{}, err
		}
		floor = derived
	}
	d := Decision{
		MinNotional:      floor,
		SlippageModelled: g.limits.SlippageBps > 0,
	}

	// The working book: a copy, so a rejected intent leaves no trace.
	pos := make(map[string]float64, len(b.Positions))
	var bookNotional float64
	for k, v := range b.Positions {
		pos[k] = v
		bookNotional += v
	}

	reject := func(in Intent, rule, detail string) {
		d.Rejected = append(d.Rejected, Rejection{Intent: in, Rule: rule, Detail: detail})
	}

	killed := b.KillSwitch != ""
	lossBreached := g.limits.DailyLossCap > 0 && b.DayPnL <= -g.limits.DailyLossCap

	for _, in := range intents {
		if in.Quantity <= 0 || in.Price <= 0 || (in.Side != cost.Buy && in.Side != cost.Sell) {
			reject(in, RuleMalformed, fmt.Sprintf("quantity %d, price %g, side %q", in.Quantity, in.Price, in.Side))
			continue
		}
		if killed {
			reject(in, RuleKillSwitch, b.KillSwitch)
			continue
		}
		// Reductions pass every remaining rule. See the package comment: a
		// cap that can trap the book in a position is worse than the position.
		if in.Side == cost.Sell {
			d.Accepted = append(d.Accepted, in)
			held := pos[in.Scrip] - in.Notional()
			if held <= 0 {
				delete(pos, in.Scrip)
			} else {
				pos[in.Scrip] = held
			}
			bookNotional -= in.Notional()
			continue
		}

		if lossBreached {
			reject(in, RuleDailyLoss, fmt.Sprintf("day P&L %.2f is at or past the %.2f cap",
				b.DayPnL, -g.limits.DailyLossCap))
			continue
		}
		n := in.Notional()
		if n < floor {
			reject(in, RuleMinNotional, fmt.Sprintf(
				"notional %.2f is below the %.2f floor; a round trip would cost more than %.2f%% of it",
				n, floor, 100*g.limits.MaxRoundTripCost))
			continue
		}
		_, held := pos[in.Scrip]
		if !held && len(pos) >= g.limits.MaxPositions {
			reject(in, RuleMaxPositions, fmt.Sprintf(
				"the book already holds %d names and %s would be new", len(pos), in.Scrip))
			continue
		}
		if b.Equity > 0 {
			after := pos[in.Scrip] + n
			if frac := after / b.Equity; frac > g.limits.MaxStockFraction {
				reject(in, RuleMaxStock, fmt.Sprintf(
					"%s would reach %.2f%% of book equity, over the %.2f%% cap",
					in.Scrip, 100*frac, 100*g.limits.MaxStockFraction))
				continue
			}
		}
		if g.limits.MaxBookNotional > 0 && bookNotional+n > g.limits.MaxBookNotional {
			reject(in, RuleMaxBookNotional, fmt.Sprintf(
				"book would reach %.2f, over the %.2f cap", bookNotional+n, g.limits.MaxBookNotional))
			continue
		}

		pos[in.Scrip] += n
		bookNotional += n
		d.Accepted = append(d.Accepted, in)
	}
	return d, nil
}

// RoundTripCost is the modelled cost of buying a notional and selling it on a
// later day, as a fraction of that notional.
//
// The DP charge lands once, on the sell, which is why the two legs go through
// cost.Day separately rather than through cost.Trade twice.
func RoundTripCost(s *cost.Schedule, date time.Time, p cost.Product, notional, slippageBps float64) (float64, error) {
	if notional <= 0 {
		return 0, fmt.Errorf("risk: notional must be positive, got %g", notional)
	}
	// Price 1 and quantity = notional: every charge here is either ad valorem
	// on turnover or flat per order or per scrip, so the split between price
	// and quantity cannot change the answer.
	qty := int64(notional)
	leg := func(d time.Time, side cost.Side) (float64, error) {
		day, err := s.Day(d, []cost.Trade{{
			Date: d, Scrip: "derivation", Product: p, Side: side, Quantity: qty, Price: 1,
		}})
		if err != nil {
			return 0, err
		}
		return day.Total(), nil
	}
	buy, err := leg(date, cost.Buy)
	if err != nil {
		return 0, err
	}
	sell, err := leg(date, cost.Sell)
	if err != nil {
		return 0, err
	}
	slip := notional * slippageBps / 10000 * 2 // both legs
	return (buy + sell + slip) / notional, nil
}

// minNotionalScanStep and minNotionalScanCap bound the search for the floor.
// A linear scan rather than a binary search because rounding each charge to
// the paisa makes the cost ratio only *nearly* monotone in notional, and a
// binary search over a nearly-monotone function lands on the wrong side of a
// step often enough to matter.
const (
	minNotionalScanStep = 100.0
	minNotionalScanCap  = 2_000_000.0
)

// MinNotional solves for the smallest position size whose modelled round-trip
// cost stays at or under maxPct of notional.
//
// This is the design's "roughly Rs10k at current charges" turned into a
// function of the charges, so it stops being right the moment they change
// rather than the moment somebody notices. Measured against the shipping
// schedule it returns roughly Rs10,000 for Zerodha and roughly three times
// that for Angel One, which is the broker decision showing up as a number.
func MinNotional(s *cost.Schedule, date time.Time, p cost.Product, maxPct, slippageBps float64) (float64, error) {
	if maxPct <= 0 {
		return 0, fmt.Errorf("risk: max round-trip cost must be positive, got %g", maxPct)
	}
	for n := minNotionalScanStep; n <= minNotionalScanCap; n += minNotionalScanStep {
		ratio, err := RoundTripCost(s, date, p, n, slippageBps)
		if err != nil {
			return 0, err
		}
		if ratio <= maxPct {
			return n, nil
		}
	}
	return 0, fmt.Errorf(
		"risk: no position up to %.0f keeps a round trip under %.3f%% on %s; the cap is unreachable at these charges",
		minNotionalScanCap, 100*maxPct, date.Format(time.DateOnly))
}

// RejectedBy returns the rejections under one rule, for a report that wants to
// say "eleven intents were below the floor" rather than listing them.
func (d Decision) RejectedBy(rule string) []Rejection {
	var out []Rejection
	for _, r := range d.Rejected {
		if r.Rule == rule {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Intent.Scrip < out[j].Intent.Scrip })
	return out
}
