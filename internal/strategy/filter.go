package strategy

import (
	"context"
	"fmt"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/engine"
)

// Stance is what the filter says about a session, and it has three values
// rather than two for a reason that cost real money before it was noticed.
//
// "The index closed below its mean" and "I could not read the index" are not
// the same statement, and collapsing them into one boolean makes a missing data
// file liquidate the entire book. NSE's index archive is missing three full
// sessions that the equity archive has -- 2014-12-15, 2015-03-12, 2015-07-08 --
// and on each of those a two-state filter would have sold everything and bought
// it back, paying roughly 0.4% round trip for a file that did not exist.
type Stance int

const (
	// RiskOn: be invested.
	RiskOn Stance = iota
	// RiskOff: the trend has broken. Liquidate and wait.
	RiskOff
	// Unknown: the filter cannot see. Hold whatever is held and open nothing
	// new. This is the only honest answer to missing data, and it is
	// deliberately NOT the same as RiskOff.
	Unknown
)

func (s Stance) String() string {
	switch s {
	case RiskOn:
		return "risk-on"
	case RiskOff:
		return "risk-off"
	default:
		return "unknown"
	}
}

// MarketFilter decides whether the book should be invested at all, read at
// every session's close.
type MarketFilter interface {
	Name() string
	// Stance reports what to do, and why. The reason is carried so a report can
	// say which sessions the strategy sat out and on what basis rather than
	// showing an unexplained flat stretch in the equity curve.
	Stance(ctx context.Context, s engine.Session) (Stance, string, error)
}

// AlwaysOn never sits out. It is the control: the design's risk-off rule is a
// hypothesis of its own, and a backtest that cannot be run without it cannot
// say what it contributed.
type AlwaysOn struct{}

func (AlwaysOn) Name() string { return "always-on" }

func (AlwaysOn) Stance(context.Context, engine.Session) (Stance, string, error) {
	return RiskOn, "no filter", nil
}

// TrendFilter is the design's 200-day rule: invested while the market index
// closes above its own N-session mean, in cash below it.
//
// **It runs on the real Nifty 50 now.** It did not always: the store held no
// index series, so the filter first ran on ICICINIFTY, the only Nifty 50 ETF in
// the equity archive that never changed ISIN. That proxy was workable and
// genuinely bad in one specific way -- it was thin before 2017 (203 of 234
// sessions in 2014 turned over less than a lakh), so a 200-day mean built from
// those sessions was averaging prices struck on almost no volume, and the
// filter could only be trusted from roughly mid-2018.
//
// Ingesting NSE's own daily index archive replaced it. The archive starts
// 2012-02-21, which is five months after the equity bhavcopy, so the filter now
// warms up in late 2012 rather than 2018 and covers essentially the whole
// backtestable window.
//
// One thing the archive does NOT give: the file carries price indices only, no
// total-return series. For a trend rule that is immaterial -- a 200-day mean
// crossing is a price question -- but for the BENCHMARK it matters, because
// comparing a strategy's total return against a price index flatters the
// strategy by the whole dividend yield. The archive publishes the index's
// trailing dividend yield in the same row, so an approximate TRI is
// constructible and clearly labelled; the exact series lives behind an
// undocumented POST endpoint that needs a session and anti-bot handling, and is
// left as an open item rather than scraped.
type TrendFilter struct {
	IndexCode string
	Days      int
}

// NewTrendFilter validates the parameters.
func NewTrendFilter(indexCode string, days int) (*TrendFilter, error) {
	if indexCode == "" {
		return nil, fmt.Errorf("strategy: trend filter needs an index code")
	}
	if days <= 1 {
		return nil, fmt.Errorf("strategy: trend filter needs more than one session, got %d", days)
	}
	return &TrendFilter{IndexCode: indexCode, Days: days}, nil
}

func (f *TrendFilter) Name() string {
	return fmt.Sprintf("trend-%dd-%s", f.Days, f.IndexCode)
}

// Stance reads the index's closes and compares the latest to its own mean.
//
// Everything it cannot answer is Unknown, never RiskOff. Warming up is not
// evidence the trend is down; neither is a session the archive is missing.
// Unknown holds the book and opens nothing, so a strategy starting cold stays
// in cash through its warm-up and a strategy already invested rides out a data
// gap instead of paying a round trip to exit and re-enter it.
func (f *TrendFilter) Stance(ctx context.Context, s engine.Session) (Stance, string, error) {
	// Reach back far enough in calendar days to contain Days sessions with
	// room for holidays: roughly 1.6 calendar days per session, plus a month.
	from := s.Date.AddDate(0, 0, -(f.Days*8/5 + 30))
	closes, err := s.Market.IndexCloses(ctx, f.IndexCode, from, s.Date)
	if err != nil {
		return Unknown, fmt.Sprintf("index %s unreadable: %v", f.IndexCode, err), nil
	}
	if len(closes) < f.Days {
		return Unknown, fmt.Sprintf("%s has %d of %d sessions; warming up",
			f.IndexCode, len(closes), f.Days), nil
	}
	window := closes[len(closes)-f.Days:]
	var sum float64
	for _, c := range window {
		sum += c.Close
	}
	mean := sum / float64(f.Days)
	last := window[len(window)-1].Close

	// The last close must be the session being decided on. An index series
	// missing today would otherwise let a stale close drive the rule, which is
	// how a gap in the archive turns into a position. NSE's archive really is
	// missing three sessions the equity archive has.
	if !window[len(window)-1].Date.Equal(s.Date) {
		return Unknown, fmt.Sprintf("%s has no close for %s; last is %s",
			f.IndexCode, s.Date.Format(time.DateOnly),
			window[len(window)-1].Date.Format(time.DateOnly)), nil
	}
	if last > mean {
		return RiskOn, fmt.Sprintf("%s %.2f above its %d-day mean %.2f", f.IndexCode, last, f.Days, mean), nil
	}
	return RiskOff, fmt.Sprintf("%s %.2f at or below its %d-day mean %.2f", f.IndexCode, last, f.Days, mean), nil
}
