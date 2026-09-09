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

## CORRECTION (2026-09-09): the numbers below were computed with a bug

Corporate actions were never applied to held positions, so a bonus issue halved
a holding instead of doubling its share count. See experiment 002's correction
for the whole account; it cost roughly 5.6 points a year here.

| | published below | corrected |
|---|---|---|
| H1, with the 200-DMA filter | −5.73% | **−0.13%** |
| control, no filter | +6.35% (N50) / +5.45% (N500) | **+9.23%** (N500) |

**The verdict does not change and its reasoning is strengthened.** H1's excess is
−0.13%, which is not positive, so the kill criterion still fires and the holdout
was still not spent on it. And the gap between the filtered rule and the same
rule unfiltered widens from about 12 points to **9.4 points of pure cost** with
none of the drawdown protection it promised — 47.6% filtered against 43.7%
unfiltered, so the filter now looks slightly WORSE on drawdown as well as on
return.

Being right for a wrong-sized reason is still being wrong about the size, which
is why this correction is here rather than left implicit.

---

## SUPERSEDED — Verdict: H1 REJECTED (in-sample, 2026-09-08)

**In-sample net excess: −5.73% against the Nifty 50, −6.63% against the Nifty
500.** The kill criterion registered above fires. **The holdout was not spent
and remains sealed.**

| | net CAGR | benchmark | excess | max DD |
|---|---|---|---|---|
| **H1, as registered (200-DMA filter)** | 6.90% | 12.63% (N50) | **−5.73%** | 45.9% |
| same, fairer benchmark | 6.90% | 13.54% (N500) | **−6.63%** | 45.9% |
| control, `AlwaysOn`, no filter | 18.99% | 12.63% (N50) | +6.35% | 49.1% |
| control, fairer benchmark | 18.99% | 13.54% (N500) | +5.45% | 49.1% |

### What killed it

**The 200-DMA filter, not the momentum rule.** The same selection without the
filter earns roughly 12 percentage points a year more. The filter left the book
fully in cash on 405 of 2,228 sessions during a market compounding at 12.6%,
liquidated 238 separate times, and paid a round trip on each. It bought the
drawdown protection it promised almost not at all: 45.9% against 49.1%.

A rule that costs twelve points a year to shave three points off a drawdown is
not a risk control, it is a tax.

### A bug found and fixed before this verdict was recorded

The first run of this experiment reported −3.77%. It was wrong, and the
correction moved the number the honest direction.

The engine recovered an intent's entity by matching its TICKER against the
session's bars. Tickers change while a position is held -- RNAM became
NAM-INDIA, ADANIGAS became ATGL, IBSEC became IBVENTURES became DHANI, each
under one unchanged ISIN -- so after a rename no bar carried the string the
holding remembered, the sell never became an order, and the position stuck in
the book permanently, marked at its last known price and inflating equity while
every later exit attempt failed silently. This project built a careful entity
system in M0 and then keyed its engine on a display label.

Intents now carry the entity id and the ticker is only a label.
`TestARenamedHoldingCanStillBeSold` is the regression.

### What the control does NOT establish

The control was registered as part of this experiment and its result is an
in-sample observation, not a validated strategy. Under the rule written at the
top of this file, a change made after seeing a result is **a new experiment**.
So `AlwaysOn` momentum becomes **H2**, and it must be registered on its own
before the holdout is touched. Three things temper it in advance:

1. **The estimate is imprecise.** Perturbing slippage by 5 basis points a leg
   moves the excess by more than two percentage points, through pure path
   dependence: different fill prices buy different quantities and therefore
   different names. Across 0, 5, 10, 25 and 50 bps the excess reads 6.35%,
   8.54%, 6.26%, 7.00% and 4.53%. The SIGN is stable over that range; the
   magnitude is not, and no single figure should be quoted as though it were.
2. **Part of it is size, not momentum.** The universe is the top 500 by
   turnover, which sits well below the Nifty 50 in market cap. Against the Nifty
   500 the excess falls from 6.35% to 5.45%, and against the Midcap 100 the gap
   narrows further still (that index compounded at 15.08% over the same period
   against the Nifty 50's 12.63%).
3. **Slippage is still zero and turnover is 5.9x a year.** That combination is
   where a momentum result is most fragile, and nothing here has measured it.

Netting the price-index adjustment (~1.2%) against the Nifty 500 comparison
leaves something in the region of **2% to 6% a year**, in-sample, before
slippage. That is a real result worth registering as H2. It is not a result
worth trading.

### An operational finding worth keeping

Raising modelled slippage does not merely reduce returns; past about 10 bps a
leg it starts colliding with RiskGate's 0.5% round-trip rule, and at 25 bps that
cap is **unreachable at any position size**. If real slippage turns out to be
that large, the rule as designed cannot be traded at all at this capital -- and
that is a finding about the design, not about the market.

### Next

H1 is closed. H2 (`AlwaysOn`) needs its own registration, its own kill criterion
and its own reading of the holdout, which is still untouched.
