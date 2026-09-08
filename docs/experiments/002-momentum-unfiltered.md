# Experiment 002: 12-1 momentum without a market filter

**Registered 2026-09-08, before the holdout had been read.** The holdout has
never been touched by any run: `verdict backtest` refuses dates at or past
2022-01-01 unless the guard is explicitly disabled, and no commit before this one
disables it.

## Where this came from, and why that matters

Experiment 001 registered 12-1 momentum **with** a 200-day trend filter, and
registered `AlwaysOn` as its control. H1 failed in-sample at −5.73% excess and
was killed without spending the holdout. The control returned +5.45% over the
Nifty 500 over the same period.

**That control result is why this experiment exists, and it is the thing most
likely to make this result untrustworthy.** The honest description: an
in-sample observation suggested a hypothesis, and the hypothesis is now being
registered before out-of-sample data is read. That is what a holdout is for and
it is legitimate.

What would NOT be legitimate is searching. Exactly two configurations have been
run in-sample — the registered rule and its registered control — plus a slippage
sensitivity on the second. No parameter has been tuned. If this file ever grows a
third variant that was picked because it looked better, the holdout is worthless
and this paragraph is the evidence.

## Hypothesis

**H2.** 12-1 cross-sectional momentum over the liquid NSE universe, top 20 equal
weighted, rebalanced monthly, with **no market-trend filter**, earns a positive
net excess return over the Nifty 500 after all trading costs.

## The rule, exactly

Identical to experiment 001 except the filter. Restated in full so this document
stands alone:

| | |
|---|---|
| Universe | top 500 by median rupee turnover over 125 sessions, point-in-time from bars alone |
| Signal | return from t−12 months to t−1 month, per entity, refusing any window that touches a succession boundary |
| Selection | top 20 by signal |
| Weighting | equal, 1% cash buffer |
| Rebalance | decided on the last session of each month, filled at the **next open** |
| Market filter | **none** |
| Broker | Zerodha schedule |
| Risk | `internal/risk` defaults; floor derived, ₹5,600 at these charges |
| Capital | ₹5,00,000 nominal |
| Slippage | 0 bps for the primary run |

## Periods

- **In-sample: 2013-01-01 to 2021-12-31.** Already read. Excess +5.45% over the
  Nifty 500, +6.35% over the Nifty 50.
- **Holdout: 2022-01-01 to 2026-09-08. Never read.** It gets **one** run. After
  that it is spent, for this hypothesis and for every other.

## Metrics

**Primary: net CAGR minus Nifty 500 CAGR.** The Nifty 500 rather than the Nifty
50, because the universe is the top 500 by turnover and sits well below the Nifty
50 in market cap; comparing against the Nifty 50 credits momentum with a size
premium it did not earn. Experiment 001 measured that difference at about 0.9
points a year.

Reported alongside, declared now so none of it is a later choice: net and gross
CAGR, excess against the Nifty 50 and Midcap 100 as well, maximum drawdown,
per-trade mean and standard deviation, turnover, total costs, and the same run
repeated at **10 and 25 bps of slippage per leg with the position floor pinned at
₹5,600**, so slippage's effect on returns is separated from its effect on what
the risk gate will admit at all.

## What counts as what

**Supported:** primary metric positive. This is consistent with H2 and is *not*
proof. One 4.7-year window is about 56 monthly rebalances, and a strategy with a
real edge can lose to its benchmark over five years without that meaning
anything.

**Not supported:** primary metric zero or negative. H2 joins H1 as rejected, and
neither it nor any variant gets another reading of this data.

**Disqualified regardless of return:** maximum drawdown above 50%. In-sample it
was 49.1%. A drawdown nobody would actually sit through is not a tradeable
result, and the honest time to write that down is before seeing the number rather
than while explaining it away.

## Limitations

All six from experiment 001 apply unchanged. Two are worse here and one is new:

- **Slippage is still zero and turnover is 5.9x a year.** This is the single
  largest unmeasured risk to the result, and it is larger without a filter
  because the book is always invested.
- **The estimate is imprecise.** Perturbing slippage by 5 bps a leg moved the
  in-sample excess by more than two points through pure path dependence. Expect
  the holdout number to carry at least that much noise.
- **New: this hypothesis was chosen after seeing in-sample data.** Registered
  before the holdout, which is the correct procedure, but it is not the same as
  having been specified in advance and the difference should not be forgotten
  when reading the result.

## What gets published

The report, either way. If the holdout says no, that is the answer and it goes in
this file next to everything above it.

## Verdict

Not yet run. The holdout has not been read.
