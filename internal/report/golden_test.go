package report_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/engine"
	"github.com/HardikSJain/verdict-machine/internal/report"
	"github.com/HardikSJain/verdict-machine/internal/risk"
	"github.com/HardikSJain/verdict-machine/internal/strategy"
)

// The golden backtest.
//
// It runs the whole stack -- point-in-time selection, the succession fence, the
// real charge schedule, the real risk gate, next-open fills -- over a frozen
// slice of the NSE archive with no database, and compares every published
// figure against a committed expectation.
//
// Its job is not to show the strategy is good. The strategy is rejected; see
// docs/experiments. Its job is to make a silent change in a number impossible:
// if a charge, a rounding rule, a selection tie-break or a fill assumption
// moves, this fails, and somebody has to decide on purpose whether the new
// number is the right one.
//
// Regenerate deliberately with: go test ./internal/report -run Golden -update
const goldenDir = "../../testdata/golden"

type goldenResult struct {
	Sessions     int     `json:"sessions"`
	Fills        int     `json:"fills"`
	Unfilled     int     `json:"unfilled"`
	RiskRejected int     `json:"risk_rejected"`
	Trades       int     `json:"trades"`
	FinalEquity  float64 `json:"final_equity"`
	NetCAGR      float64 `json:"net_cagr"`
	BenchCAGR    float64 `json:"benchmark_cagr"`
	ExcessCAGR   float64 `json:"excess_cagr"`
	MaxDrawdown  float64 `json:"max_drawdown"`
	TotalCosts   float64 `json:"total_costs"`
	TradeMean    float64 `json:"trade_mean"`
	TradeStdDev  float64 `json:"trade_stddev"`
	Turnover     float64 `json:"turnover"`
	MinNotional  float64 `json:"min_notional"`
}

func runGolden(t *testing.T) goldenResult {
	t.Helper()
	f, bench, err := engine.LoadGolden(goldenDir)
	require.NoError(t, err)

	sched, err := cost.NewSchedule(cost.Zerodha)
	require.NoError(t, err)
	broker, err := engine.NewPaper(sched, 0)
	require.NoError(t, err)
	gate, err := risk.NewGate(risk.DefaultLimits(), sched)
	require.NoError(t, err)
	// Top 12 of the fixture's 15 names. Twelve rather than five because equal
	// weight across five is 20% a name against RiskGate's 10% single-stock cap,
	// and the first version of this test produced zero fills and 115 risk
	// rejections — the gate refusing a concentrated book, correctly. Twelve puts
	// each position at 8.25% and still drops three names, so selection is doing
	// something.
	strat, err := strategy.NewMomentum(12, 12, 1, strategy.AlwaysOn{})
	require.NoError(t, err)
	e, err := engine.New(f, broker, gate, strat)
	require.NoError(t, err)

	res, err := e.Run(context.Background(), 500000)
	require.NoError(t, err)
	b := report.Build(res, "NIFTY500", bench, 500000)

	return goldenResult{
		Sessions: b.Sessions, Fills: b.Fills, Unfilled: b.Unfilled,
		RiskRejected: b.RiskRejected, Trades: len(b.Trades),
		FinalEquity: round4(b.FinalNet), NetCAGR: round6(b.NetCAGR),
		BenchCAGR: round6(b.BenchmarkCAGR), ExcessCAGR: round6(b.ExcessCAGR),
		MaxDrawdown: round6(b.MaxDrawdown), TotalCosts: round4(b.TotalCosts),
		TradeMean: round6(b.TradeMean), TradeStdDev: round6(b.TradeStdDev),
		Turnover: round6(b.Turnover), MinNotional: b.MinNotional,
	}
}

// TestGoldenBacktestIsReproducible: two runs of identical inputs must produce
// identical outputs. Map iteration order, a tie broken by chance, or a
// concurrent read would each break this before they broke anything a human
// would notice.
func TestGoldenBacktestIsReproducible(t *testing.T) {
	a := runGolden(t)
	b := runGolden(t)
	require.Equal(t, a, b, "the same inputs must give the same answer, every time")
}

// TestGoldenBacktestMatchesTheCommittedResult.
func TestGoldenBacktestMatchesTheCommittedResult(t *testing.T) {
	got := runGolden(t)
	path := filepath.Join(goldenDir, "expected.json")

	if os.Getenv("UPDATE_GOLDEN") != "" {
		b, err := json.MarshalIndent(got, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(b, '\n'), 0o644))
		t.Log("golden result rewritten; the diff is the change you are making")
		return
	}

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "run with UPDATE_GOLDEN=1 to create it")
	var want goldenResult
	require.NoError(t, json.Unmarshal(raw, &want))
	require.Equal(t, want, got,
		"a published figure moved. That is not necessarily wrong, but it is never accidental: "+
			"decide whether the new number is right, then rerun with UPDATE_GOLDEN=1")
}

func round4(v float64) float64 { return float64(int64(v*1e4+0.5)) / 1e4 }
func round6(v float64) float64 {
	if v < 0 {
		return -float64(int64(-v*1e6+0.5)) / 1e6
	}
	return float64(int64(v*1e6+0.5)) / 1e6
}
