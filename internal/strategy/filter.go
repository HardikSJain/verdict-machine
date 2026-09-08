package strategy

import (
	"context"
	"fmt"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/engine"
)

// MarketFilter decides whether the book should be invested at all, read at
// every session's close.
type MarketFilter interface {
	Name() string
	// RiskOn reports whether to be invested, and why. The reason is carried so
	// a report can say which sessions the strategy sat out and on what basis
	// rather than showing an unexplained flat stretch in the equity curve.
	RiskOn(ctx context.Context, s engine.Session) (bool, string, error)
}

// AlwaysOn never sits out. It is the control: the design's risk-off rule is a
// hypothesis of its own, and a backtest that cannot be run without it cannot
// say what it contributed.
type AlwaysOn struct{}

func (AlwaysOn) Name() string { return "always-on" }

func (AlwaysOn) RiskOn(context.Context, engine.Session) (bool, string, error) {
	return true, "no filter", nil
}

// TrendFilter is the design's 200-day rule: invested while the market proxy
// closes above its own N-session mean, in cash below it.
//
// **It runs on a proxy, and the proxy is not the Nifty 50.** The store holds
// no index series -- NSE publishes indices in a separate file this project
// does not ingest -- so the filter reads a Nifty 50 ETF instead. The design
// anticipated this and named it as an open question; what follows is what the
// substitution actually costs, measured rather than assumed.
//
// ICICINIFTY (entity 100 on the live store) is the only Nifty 50 tracker in
// the archive that never changed ISIN: one entity, 2013-04-05 to 2026-09-07,
// no succession boundary to refuse across. NIFTYBEES would be the obvious
// choice and cannot be used -- it splits into two unlinked entities at
// 2019-12-20, the Benchmark to Reliance Nippon handover, and eight years of
// its history are invisible until somebody hand-writes a roster line.
//
// Three differences from the index itself, all of which push the same way:
//
//   - It is an ETF price, so it carries tracking error and trades at a small
//     premium or discount to NAV.
//   - It accumulates dividends, so it drifts ABOVE the price index over time
//     and sits nearer a total-return series than the Nifty 50 does.
//   - **It was thin before 2017.** Measured on the live store: 203 of 234
//     sessions in 2014 turned over less than a lakh, 175 of 248 in 2015, 61 of
//     247 in 2016, and none at all from 2017 onward. A close struck on
//     fifty thousand rupees of turnover is not a reliable read on where the
//     market closed, so a 200-day mean built from those sessions is not either.
//
// So this filter is trustworthy from 2017, and with its own warm-up that means
// **a backtest using it is honest from roughly mid-2018**. Over the archive's
// earlier years, run AlwaysOn and say so.
type TrendFilter struct {
	ProxyEntityID int64
	ProxyName     string
	Days          int
}

// NewTrendFilter validates the parameters.
func NewTrendFilter(entityID int64, name string, days int) (*TrendFilter, error) {
	if entityID <= 0 {
		return nil, fmt.Errorf("strategy: trend filter needs a proxy entity id")
	}
	if days <= 1 {
		return nil, fmt.Errorf("strategy: trend filter needs more than one session, got %d", days)
	}
	return &TrendFilter{ProxyEntityID: entityID, ProxyName: name, Days: days}, nil
}

func (f *TrendFilter) Name() string {
	return fmt.Sprintf("trend-%dd-%s", f.Days, f.ProxyName)
}

// RiskOn reads the proxy's closes and compares the latest to its own mean.
//
// **A short history means risk OFF, not risk on.** Warming up, a gap in the
// proxy, a boundary the store refuses to average across: none of these are
// evidence that the trend is up, and defaulting to invested would make the
// filter silently absent for exactly the stretch where nobody checked it.
// The equity curve then shows a flat opening stretch, which is the honest
// picture of a strategy that could not yet see.
func (f *TrendFilter) RiskOn(ctx context.Context, s engine.Session) (bool, string, error) {
	// Reach back far enough in calendar days to contain Days sessions with
	// room for holidays: roughly 1.6 calendar days per session, plus a month.
	from := s.Date.AddDate(0, 0, -(f.Days*8/5 + 30))
	closes, err := s.Market.Closes(ctx, f.ProxyEntityID, from, s.Date)
	if err != nil {
		return false, fmt.Sprintf("proxy %s unreadable: %v", f.ProxyName, err), nil
	}
	if len(closes) < f.Days {
		return false, fmt.Sprintf("%s has %d of %d sessions; warming up",
			f.ProxyName, len(closes), f.Days), nil
	}
	window := closes[len(closes)-f.Days:]
	var sum float64
	for _, c := range window {
		sum += c.Close
	}
	mean := sum / float64(f.Days)
	last := window[len(window)-1].Close

	// The last close must be the session being decided on. A proxy that did
	// not trade today would otherwise let a stale close drive the rule, and a
	// stale close is exactly what a halted or delisted proxy produces.
	if !window[len(window)-1].Date.Equal(s.Date) {
		return false, fmt.Sprintf("%s did not trade on %s; last close is %s",
			f.ProxyName, s.Date.Format(time.DateOnly),
			window[len(window)-1].Date.Format(time.DateOnly)), nil
	}
	if last > mean {
		return true, fmt.Sprintf("%s %.2f above its %d-day mean %.2f", f.ProxyName, last, f.Days, mean), nil
	}
	return false, fmt.Sprintf("%s %.2f at or below its %d-day mean %.2f", f.ProxyName, last, f.Days, mean), nil
}
