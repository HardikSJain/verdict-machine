package strategy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/HardikSJain/verdict-machine/internal/engine"
)

// A Ranker turns a point-in-time universe into an ordered shortlist, best
// first. It is the only thing that differs between the strategies registered in
// docs/experiments; cadence, sizing, the risk gate and next-open fills are
// shared, so two rules can be compared without wondering whether the harness
// treated them differently.
//
// Every Ranker must also report what it could NOT rank. A name the succession
// fence refused, or one with too little history, has to reach the journal — a
// backtest that silently ranks a smaller universe than it claims is choosing
// its own candidates.
type Ranker interface {
	Name() string
	// Rank returns entity ids, best first, plus the ones it had to drop and why.
	Rank(ctx context.Context, s engine.Session, universe []engine.Member) (ranked []int64, refused, absent map[int64]string, err error)
}

// byScore sorts entities by a score, breaking ties on entity id.
//
// The tie-break is not decoration. Ranging a Go map is deliberately randomised,
// so two runs of an identical backtest would pick different names whenever two
// scores matched, and the difference would surface as unexplained variance in a
// result nobody could reproduce.
func byScore(scores map[int64]float64, ascending bool) []int64 {
	out := make([]int64, 0, len(scores))
	for id := range scores {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := scores[out[i]], scores[out[j]]
		if a != b {
			if ascending {
				return a < b
			}
			return a > b
		}
		return out[i] < out[j]
	})
	return out
}

func universeIDs(u []engine.Member) []int64 {
	ids := make([]int64, 0, len(u))
	for _, m := range u {
		ids = append(ids, m.EntityID)
	}
	return ids
}

// ReturnRanker ranks by the return over a window, either direction.
//
// Momentum and short-term reversal are the same computation read two ways, so
// they are one type. Momentum is (12, 1, descending): rank on the year to a
// month ago, skipping the most recent month because momentum reverses at that
// horizon. Reversal is (1, 0, ascending): rank on that skipped month alone and
// buy the worst of it, which is the phenomenon RSI encodes.
type ReturnRanker struct {
	FormationMonths int
	SkipMonths      int
	// Ascending picks the WORST performers. Reversal is momentum's opposite,
	// not a different measurement.
	Ascending bool
	Label     string
}

func (r ReturnRanker) Name() string { return r.Label }

func (r ReturnRanker) Rank(ctx context.Context, s engine.Session, universe []engine.Member) ([]int64, map[int64]string, map[int64]string, error) {
	if r.FormationMonths <= r.SkipMonths {
		return nil, nil, nil, fmt.Errorf("strategy: formation %d must exceed skip %d", r.FormationMonths, r.SkipMonths)
	}
	from := s.Date.AddDate(0, -r.FormationMonths, 0)
	to := s.Date.AddDate(0, -r.SkipMonths, 0)
	rs, err := s.Market.Returns(ctx, universeIDs(universe), from, to)
	if err != nil {
		return nil, nil, nil, err
	}
	return byScore(rs.Priced, r.Ascending), rs.Refused, rs.Absent, nil
}

// VolatilityRanker ranks by trailing volatility, lowest first.
//
// The low-volatility anomaly is documented across many markets and its usual
// explanation — investors who cannot use leverage bid up high-beta names
// instead — has nothing to do with momentum's. That independence is the point
// of including it: two rules that fail for the same reason teach one lesson
// between them.
type VolatilityRanker struct {
	LookbackMonths int
	Label          string
}

func (v VolatilityRanker) Name() string { return v.Label }

func (v VolatilityRanker) Rank(ctx context.Context, s engine.Session, universe []engine.Member) ([]int64, map[int64]string, map[int64]string, error) {
	if v.LookbackMonths <= 0 {
		return nil, nil, nil, fmt.Errorf("strategy: volatility lookback must be positive, got %d", v.LookbackMonths)
	}
	from := s.Date.AddDate(0, -v.LookbackMonths, 0)
	vols, skipped, err := s.Market.Volatility(ctx, universeIDs(universe), from, s.Date)
	if err != nil {
		return nil, nil, nil, err
	}
	// The store reports a fence refusal and a short history through one map;
	// both are "could not rank", and the message says which.
	refused, absent := map[int64]string{}, map[int64]string{}
	for id, why := range skipped {
		if strings.Contains(why, "succession guard band") {
			refused[id] = why
		} else {
			absent[id] = why
		}
	}
	return byScore(vols, true), refused, absent, nil
}

// TurnoverRanker keeps the universe's own order, which is descending median
// rupee turnover.
//
// It is the yardstick rather than a strategy. Holding the most liquid names
// equal weighted isolates how much of any other rule's result comes from equal
// weighting and this universe's size tilt rather than from a signal. A rule that
// cannot beat this one has contributed nothing but its costs.
type TurnoverRanker struct{}

func (TurnoverRanker) Name() string { return "turnover" }

func (TurnoverRanker) Rank(_ context.Context, _ engine.Session, universe []engine.Member) ([]int64, map[int64]string, map[int64]string, error) {
	return universeIDs(universe), map[int64]string{}, map[int64]string{}, nil
}

// The three rules registered in docs/experiments/003-three-cheap-rules.md.
//
// Each is a constructor rather than a type, because the only thing that
// distinguishes them is a ranker and a cadence. Anything else that differed
// would make the comparison between them meaningless.

// NewLowVolatility is H3: hold the least volatile names, rebalanced annually.
//
// Annual on purpose. Cost is the binding constraint on every rule tested in this
// repository -- experiment 002 paid 6.7% of capital at 7.4x turnover -- and a
// rule trading once a year starts more than a point ahead of one trading twelve
// times.
func NewLowVolatility(top, lookbackMonths int, f MarketFilter) (*Selector, error) {
	label := fmt.Sprintf("lowvol-%dm-top%d-annual", lookbackMonths, top)
	return New(label, VolatilityRanker{LookbackMonths: lookbackMonths, Label: label}, top, 12, f)
}

// NewShortTermReversal is H4: hold last month's WORST performers, rebalanced
// monthly.
//
// It is momentum's exact opposite, and it is the phenomenon RSI encodes.
// Momentum skips its most recent month because that month reverses; this owns
// the skipped month alone. Turnover is necessarily high, so it has to earn a
// great deal to survive its own costs -- which is itself the test.
func NewShortTermReversal(top int, f MarketFilter) (*Selector, error) {
	label := fmt.Sprintf("reversal-1m-top%d", top)
	return New(label, ReturnRanker{
		FormationMonths: 1, SkipMonths: 0, Ascending: true, Label: label,
	}, top, 1, f)
}

// NewEqualWeightUniverse is H5: hold the most traded names, equal weighted,
// rebalanced annually.
//
// Barely a strategy, and that is the point. It is the yardstick that isolates
// how much of any other rule's result comes from equal weighting and this
// universe's size tilt rather than from a signal. A rule that cannot beat this
// one has contributed nothing but its costs.
func NewEqualWeightUniverse(top int, f MarketFilter) (*Selector, error) {
	label := fmt.Sprintf("equalweight-top%d-annual", top)
	return New(label, TurnoverRanker{}, top, 12, f)
}
