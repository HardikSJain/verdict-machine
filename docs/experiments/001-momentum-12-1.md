# Experiment 001: 12-1 cross-sectional momentum on NSE

**Registered 2026-09-08, before any backtest of this rule had been run.**

That claim is checkable rather than asserted: this file is committed and pushed
to a public repository, and the commit that adds it precedes every commit that
can produce a result. `verdict backtest` does not exist yet at the moment this
is written. If you are reading the git history and find a backtest commit dated
earlier, this registration is worthless and should be treated as such.

## Hypothesis

**H1.** Ranking the liquid NSE universe by its return from twelve months ago to
one month ago, holding the top 20 equal-weighted, and rebalancing monthly earns
a positive excess return over the Nifty 50 **after all trading costs**.

The null is that it does not: that any gross edge in cross-sectional momentum is
consumed by STT, brokerage, stamp duty, GST, depository charges and turnover.
That null is the outcome to expect. The reference project this design started
from ran 38 experiments and found no edge after costs in any of them.

## The rule, exactly

| | |
|---|---|
| Universe | top 500 by median rupee turnover over 125 sessions, computed point-in-time from bars alone |
| Signal | return from t−12 months to t−1 month, per entity |
| Skip | the most recent month is excluded, because cross-sectional momentum reverses at that horizon |
| Selection | top 20 by signal |
| Weighting | equal, 1% cash buffer |
| Rebalance | decided on the last session of each month, filled at the **next open** |
| Market filter | Nifty 50 below its 200-session mean → liquidate and wait; unreadable → hold, trade nothing |
| Broker | Zerodha schedule |
| Costs | `internal/cost`, every charge, DP included |
| Risk | `internal/risk` defaults: 20 positions, 10% single-stock cap, derived minimum notional |
| Capital | ₹5,00,000 nominal |

Any change to these after a result is seen is a **new experiment with a new
number**, not an edit to this one.

## Periods

- **In-sample: 2013-01-01 to 2021-12-31.** Roughly 108 monthly rebalances.
  Explore this freely. Iterate, fix, re-run as often as useful.
- **Holdout: 2022-01-01 onward. Sealed.** Roughly 56 rebalances. It gets **one**
  run, after provenance exists, and whatever it says is the result.

2022 was chosen because the holdout then spans a genuinely different regime: the
2022 drawdown, the 2023–24 rally, and the current market.

## Metrics

**Primary: net CAGR minus Nifty 50 CAGR over the same period**, after every
modelled cost.

Secondary, all reported whatever the primary says: gross CAGR, maximum drawdown,
per-trade mean and standard deviation, annual turnover, total cost as a fraction
of starting capital, months held in cash, and the count of names the succession
fence refused.

## Kill criterion

**If in-sample net excess return is not positive, H1 is dead and the holdout is
never spent on it.** A rule that cannot beat buying the index during the period
it was developed against has nothing that out-of-sample data could confirm.

Spending the holdout on a rule that already failed in-sample is the single
easiest way to burn the only clean data there is, so it is written down here
rather than decided in the moment.

## Limitations, declared before the result

These are stated now so they cannot be deployed selectively afterwards. Every
one of them is a reason a good result might be overstated.

1. **The sample is small.** About 150 monthly observations in total and 56 in the
   holdout. This can show a rule is not obviously broken. It cannot establish
   that a modest edge is real.
2. **Slippage is zero.** Nothing has measured it. Fills are at the open with no
   market impact, so every result is optimistic by an unmeasured amount. The
   promotion gate in M4 is what measures it, on real fills.
3. **Some charge rates did not exist in the earlier years.** GST at 18% is
   applied before July 2017, when service tax applied at a lower rate, and the
   uniform 0.015% stamp duty is applied before July 2020, when duty was levied
   state by state with no single national rate. Both are flagged per trade and
   the report prints the count.
4. **The benchmark is a price index, not a total-return index.** NSE's archive
   publishes no TRI. A price benchmark understates the market by its dividend
   yield, roughly 1.2% a year, and that difference **flatters the strategy**. If
   the measured excess return is smaller than that, it is not an excess return.
5. **No read-time adjustment layer exists**, so the succession fence refuses any
   name whose ranking window touches a corporate action. Measured at one date
   that was 12 of 500. Those names are excluded from selection, not mispriced.
6. **One broker, one schedule.** Results are Zerodha's costs. At Angel One the
   same trades cost roughly twice as much.

## What gets published

The report, either way. A null result is the expected outcome and is worth as
much as a positive one; it is also the outcome this whole apparatus was built to
be able to believe.

## Verdict

Not yet run.
