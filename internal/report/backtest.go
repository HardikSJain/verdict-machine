// Package report turns an engine result into the numbers experiment 001 says
// must be published, whichever way they come out.
//
// Everything here is descriptive. Nothing decides whether a strategy is good;
// that judgement belongs to the registered kill criterion and to a human
// reading the caveats printed beside the figures. The caveats are printed
// BESIDE the figures on purpose: a report that puts its limitations in a
// separate document is a report whose limitations do not get read.
package report

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/engine"
)

// Trade is one completed round trip: shares bought and later sold, with the
// charges of both legs already deducted.
type Trade struct {
	Scrip    string
	Exit     time.Time
	Basis    float64 // what the shares cost, charges included
	Proceeds float64 // what they returned, charges deducted
	Return   float64
}

// Backtest is the published summary.
type Backtest struct {
	Strategy  string
	From, To  time.Time
	Sessions  int
	Years     float64
	StartCash float64
	FinalNet  float64

	NetCAGR       float64
	GrossCAGR     float64
	BenchmarkCAGR float64
	ExcessCAGR    float64 // the primary metric: NetCAGR - BenchmarkCAGR

	MaxDrawdown  float64
	TotalCosts   float64
	CostFraction float64 // costs as a fraction of starting capital
	Turnover     float64 // annualised, traded value over average equity

	Trades       []Trade
	TradeMean    float64
	TradeStdDev  float64
	TradeWinRate float64

	SessionsInCash int
	Fills          int
	Unfilled       int
	// UnfilledBy groups the reasons. A high unfilled count is the difference
	// between a strategy that was tested and one that was merely run, so the
	// reason has to be visible next to the result rather than a page away.
	UnfilledBy   map[string]int
	RiskRejected int
	Unverified   map[cost.Component]int

	// BenchmarkIsPriceIndex records that the benchmark carries no dividends,
	// which understates it by roughly the index's yield and therefore
	// overstates ExcessCAGR by the same amount. Experiment 001 declares this
	// as limitation 4; the report repeats it so a reader of the number cannot
	// miss it.
	Benchmark             string
	BenchmarkIsPriceIndex bool
	SlippageModelled      bool
	MinNotional           float64
}

// Build computes the summary from an engine result and a benchmark series.
func Build(res engine.Result, benchmarkName string, benchmark []engine.DatedClose, startCash float64) Backtest {
	b := Backtest{
		Strategy: res.Strategy, From: res.From, To: res.To, Sessions: res.Sessions,
		StartCash: startCash, TotalCosts: res.TotalCosts,
		Fills: len(res.Fills), Unfilled: len(res.Unfilled),
		RiskRejected: len(res.Rejected), Unverified: res.Unverified,
		SlippageModelled: res.SlippageModelled, MinNotional: res.MinNotional,
		Benchmark: benchmarkName, BenchmarkIsPriceIndex: true,
	}
	if len(res.Equity) == 0 {
		return b
	}
	b.FinalNet = res.Equity[len(res.Equity)-1].Equity
	b.Years = res.To.Sub(res.From).Hours() / 24 / 365.25
	b.NetCAGR = cagr(startCash, b.FinalNet, b.Years)

	// Gross is first order only: the same trades with their charges added back,
	// ignoring what those charges would themselves have compounded into. It is
	// reported to show how much of the result the costs consumed, not as a
	// second strategy.
	b.GrossCAGR = cagr(startCash, b.FinalNet+res.TotalCosts, b.Years)
	if startCash > 0 {
		b.CostFraction = res.TotalCosts / startCash
	}

	peak := res.Equity[0].Equity
	for _, p := range res.Equity {
		if p.Equity > peak {
			peak = p.Equity
		}
		if peak > 0 {
			if dd := 1 - p.Equity/peak; dd > b.MaxDrawdown {
				b.MaxDrawdown = dd
			}
		}
		if p.Held == 0 {
			b.SessionsInCash++
		}
	}

	if len(benchmark) >= 2 {
		first, last := benchmark[0].Close, benchmark[len(benchmark)-1].Close
		b.BenchmarkCAGR = cagr(first, last, b.Years)
	}
	b.ExcessCAGR = b.NetCAGR - b.BenchmarkCAGR

	b.UnfilledBy = map[string]int{}
	for _, u := range res.Unfilled {
		b.UnfilledBy[classify(u.Reason)]++
	}
	b.Trades = roundTrips(res.Fills)
	b.TradeMean, b.TradeStdDev, b.TradeWinRate = tradeStats(b.Trades)
	b.Turnover = turnover(res, b.Years)
	return b
}

// classify collapses an unfilled reason to its kind, so a thousand
// per-order messages become three counts.
func classify(reason string) string {
	switch {
	case strings.Contains(reason, "insufficient cash"):
		return "insufficient cash"
	case strings.Contains(reason, "did not trade"):
		return "name did not trade at the open"
	case strings.Contains(reason, "no bar on the deciding session"):
		return "no bar on the deciding session"
	default:
		return reason
	}
}

func cagr(from, to, years float64) float64 {
	if from <= 0 || to <= 0 || years <= 0 {
		return 0
	}
	return math.Pow(to/from, 1/years) - 1
}

// roundTrips pairs fills into completed trades using the same average-cost rule
// the portfolio uses, so a position built or exited in clips produces one trade
// per exit rather than one per order.
//
// A position still open at the end of the run is NOT a trade and is excluded.
// Counting it would mean marking an unrealised position as a result, which is
// the most common way a per-trade statistic gets flattered.
func roundTrips(fills []engine.Fill) []Trade {
	type holding struct {
		qty   int64
		basis float64
	}
	held := map[int64]*holding{}
	var out []Trade

	ordered := append([]engine.Fill(nil), fills...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Date.Before(ordered[j].Date) })

	for _, f := range ordered {
		h := held[f.EntityID]
		if h == nil {
			h = &holding{}
			held[f.EntityID] = h
		}
		switch f.Side {
		case cost.Buy:
			h.qty += f.Quantity
			h.basis += f.Value() + f.Charges.Total()
		case cost.Sell:
			if h.qty < f.Quantity || h.qty == 0 {
				continue
			}
			basisOut := h.basis * float64(f.Quantity) / float64(h.qty)
			proceeds := f.Value() - f.Charges.Total()
			h.qty -= f.Quantity
			h.basis -= basisOut
			if basisOut > 0 {
				out = append(out, Trade{
					Scrip: f.Scrip, Exit: f.Date, Basis: basisOut,
					Proceeds: proceeds, Return: proceeds/basisOut - 1,
				})
			}
		}
	}
	return out
}

func tradeStats(ts []Trade) (mean, sd, winRate float64) {
	if len(ts) == 0 {
		return 0, 0, 0
	}
	var wins int
	for _, t := range ts {
		mean += t.Return
		if t.Return > 0 {
			wins++
		}
	}
	mean /= float64(len(ts))
	for _, t := range ts {
		d := t.Return - mean
		sd += d * d
	}
	// Sample standard deviation: these trades are a sample of the rule's
	// behaviour, not the population of everything it will ever do.
	if len(ts) > 1 {
		sd = math.Sqrt(sd / float64(len(ts)-1))
	} else {
		sd = 0
	}
	return mean, sd, float64(wins) / float64(len(ts))
}

func turnover(res engine.Result, years float64) float64 {
	if years <= 0 || len(res.Equity) == 0 {
		return 0
	}
	var traded, equitySum float64
	for _, f := range res.Fills {
		traded += f.Value()
	}
	for _, p := range res.Equity {
		equitySum += p.Equity
	}
	avgEquity := equitySum / float64(len(res.Equity))
	if avgEquity <= 0 {
		return 0
	}
	return traded / avgEquity / years
}

// String renders the report. The caveats are part of it, not an appendix.
func (b Backtest) String() string {
	var s strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&s, f, a...) }

	p("%s\n", b.Strategy)
	p("%s .. %s  (%d sessions, %.2f years)\n\n",
		b.From.Format(time.DateOnly), b.To.Format(time.DateOnly), b.Sessions, b.Years)

	p("  starting capital        %14s\n", rupees(b.StartCash))
	p("  final equity            %14s\n", rupees(b.FinalNet))
	p("  net CAGR                %13.2f%%\n", 100*b.NetCAGR)
	p("  gross CAGR (1st order)  %13.2f%%\n", 100*b.GrossCAGR)
	p("  benchmark %-13s %13.2f%%\n", b.Benchmark, 100*b.BenchmarkCAGR)
	p("  EXCESS (primary metric) %13.2f%%\n\n", 100*b.ExcessCAGR)

	p("  max drawdown            %13.2f%%\n", 100*b.MaxDrawdown)
	p("  total costs             %14s  (%.2f%% of starting capital)\n",
		rupees(b.TotalCosts), 100*b.CostFraction)
	p("  turnover                %13.2f x/yr\n", b.Turnover)
	p("  sessions fully in cash  %14d of %d\n\n", b.SessionsInCash, b.Sessions)

	p("  completed round trips   %14d\n", len(b.Trades))
	p("  per-trade mean          %13.2f%%\n", 100*b.TradeMean)
	p("  per-trade std dev       %13.2f%%\n", 100*b.TradeStdDev)
	p("  win rate                %13.2f%%\n\n", 100*b.TradeWinRate)

	p("  fills %d, unfilled %d, risk-rejected %d, min notional %s\n",
		b.Fills, b.Unfilled, b.RiskRejected, rupees(b.MinNotional))
	if len(b.UnfilledBy) > 0 {
		var kinds []string
		for k := range b.UnfilledBy {
			kinds = append(kinds, k)
		}
		sort.Slice(kinds, func(i, j int) bool { return b.UnfilledBy[kinds[i]] > b.UnfilledBy[kinds[j]] })
		for _, k := range kinds {
			p("    unfilled: %-34s %6d\n", k, b.UnfilledBy[k])
		}
	}

	p("\ncaveats that apply to every number above:\n")
	if !b.SlippageModelled {
		p("  * SLIPPAGE IS ZERO. Nothing has measured it, so fills are at the open\n")
		p("    with no market impact and every figure here is optimistic by an\n")
		p("    unmeasured amount.\n")
	}
	if b.BenchmarkIsPriceIndex {
		p("  * The benchmark %s is a PRICE index, not total return. It\n", b.Benchmark)
		p("    excludes dividends, roughly 1.2%% a year, so it understates the market\n")
		p("    and OVERSTATES the excess above by about that much. An excess smaller\n")
		p("    than the dividend yield is not an excess.\n")
	}
	if len(b.Unverified) > 0 {
		var comps []string
		for c, n := range b.Unverified {
			comps = append(comps, fmt.Sprintf("%s x%d", c, n))
		}
		sort.Strings(comps)
		p("  * Charges leaning on rates no document in this repository reconciles:\n")
		p("    %s\n", strings.Join(comps, ", "))
	}
	return s.String()
}

func rupees(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	parts := strings.SplitN(s, ".", 2)
	n := parts[0]
	// Indian grouping: last three digits, then pairs.
	var grouped string
	if len(n) > 3 {
		head, tail := n[:len(n)-3], n[len(n)-3:]
		var chunks []string
		for len(head) > 2 {
			chunks = append([]string{head[len(head)-2:]}, chunks...)
			head = head[:len(head)-2]
		}
		if head != "" {
			chunks = append([]string{head}, chunks...)
		}
		grouped = strings.Join(chunks, ",") + "," + tail
	} else {
		grouped = n
	}
	out := "₹" + grouped + "." + parts[1]
	if neg {
		return "-" + out
	}
	return out
}
