# Experiment 003: three cheap rules, registered together

**Registered 2026-09-09, before any of the three was implemented or run.** None
of `LowVolatility`, `ShortTermReversal` or `EqualWeightAll` exists in the
codebase at this commit. The ordering is checkable in the history rather than
asserted here.

## Why three at once, and why that costs something

Experiments 001 and 002 spent the clean holdout. There is no untouched period
left: 2013–2021 was explored, 2022–2026 was read once and is recorded as spent
in the `runs` table, and the archive before 2013 is about sixteen months — less
than one formation window.

So this experiment cannot offer a virgin holdout, and pretending otherwise would
be worse than admitting it. What it offers instead is the two protections that
are still available.

**All three are declared before any is run.** The failure mode that matters here
is not overfitting a parameter, it is searching: testing ten rules, keeping the
one that looked good, and reporting it as though it were the only one tried. The
count is fixed at three, in writing, now. Every one of them is reported.

**None of them has a fitted parameter.** This is what makes running fixed rules
over already-seen data less bad than it sounds. There is nothing to tune, no
lookback chosen because it scored well, no threshold optimised. A rule with no
knobs cannot be quietly fitted to the period it runs on. The residual
contamination is that these three were *chosen* by someone who has seen how this
market behaved — real, unavoidable now, and stated rather than hidden.

## Why these three, and not indicators

Every classic technical indicator is a transformation of the same one input.
MACD is the difference of two moving averages — a trend measure. RSI is a
normalised ratio of up moves to down moves — short-term reversal. Bollinger
Bands are price against its own volatility. Trend, reversal, volatility: three
phenomena, and each of the rules below tests one of them directly rather than
through a parameterisation somebody settled on in 1978.

Trend is already tested and rejected: that was momentum, in experiments 001
and 002.

## The single biggest input to the design

Experiment 002's turnover was 7.4x a year and its costs were 6.7% of starting
capital over 4.7 years. **Cost is the binding constraint, not signal quality**,
and two of these three rebalance annually for that reason. A rule trading once a
year pays roughly 0.3% where a monthly rule pays 1.5–2%, which is a head start of
more than a point before the signal does anything at all.

## The three hypotheses

**H3 — Low volatility, annual.** Rank the universe by the standard deviation of
daily returns over the trailing 12 months. Hold the 20 lowest, equal weighted,
rebalanced once a year on the last session of December. The low-volatility
anomaly is documented across many markets and its mechanism (leverage
constraints pushing demand into high-beta names) is unrelated to momentum's.

**H4 — Short-term reversal, monthly.** Rank by the trailing 1-month return and
hold the 20 *worst*, equal weighted, rebalanced monthly. This is the exact
opposite of what momentum bought, and it is the phenomenon RSI encodes. Momentum
skips its most recent month precisely because that month reverses; this tests
whether the reversal is worth owning on its own. Turnover will be high, so it
has to earn a great deal to survive.

**H5 — Equal weight the universe, annual.** Hold the top 100 by turnover, equal
weighted, rebalanced annually. Barely a strategy. It is the yardstick: it
isolates how much of any result above comes from equal weighting and the size
tilt of this universe rather than from a signal. If H3 or H4 fails to beat H5,
its signal contributed nothing.

Common to all three: the same universe, costs, risk gate and next-open fills as
experiments 001 and 002. Zerodha schedule, ₹5,00,000 nominal, 20 positions
(100 for H5), the succession fence refusing any window touching a corporate
action.

## Period

**2013-01-01 to 2026-09-08.** The whole usable archive, run once per rule.
No holdout, for the reason given above.

## The bar, raised because there are three

A single positive number is not a result when three rules were tried. The
pre-declared bar is **economic rather than statistical**, because 150 monthly
observations will not support a p-value anybody should believe:

A rule is **supported** only if its net excess over the Nifty 500 is positive
after **all three** of these adjustments applied together:

1. **Nifty 500 as the benchmark**, not the Nifty 50 — this universe sits below
   the large-cap index and comparing against it credits a size premium the rule
   did not earn.
2. **Minus 1.2 percentage points** for the benchmark being a price index. NSE
   publishes no total-return series here, so the benchmark excludes dividends
   and flatters every strategy measured against it.
3. **At 10 basis points of slippage per leg.** Nothing has measured real
   slippage; 10 bps is modest for this universe, and experiment 002 turned
   negative at exactly this assumption.

Anything that clears all three is worth a second look. Anything that clears only
the raw comparison is what experiment 002 already was: the most flattering
framing of a result that is not there.

**Disqualified regardless of return:** maximum drawdown above 50%.

## What is reported

All three rules, every one of them, whatever they say. For each: net and gross
CAGR, excess against the Nifty 500, Nifty 50 and Midcap 100, the same at 0, 10
and 25 bps of slippage, maximum drawdown, per-trade mean and standard deviation,
turnover, total costs, and the count of names the succession fence refused.

## Limitations

The six declared in experiment 001 apply unchanged. Three more apply here:

1. **No holdout exists.** This is the weakest evidence any experiment in this
   repository has produced, and no result from it should be traded on. At most
   it says which direction is worth spending future data on.
2. **Three rules were tried.** With three tests and a noisy estimate — experiment
   002 moved more than two percentage points when slippage was perturbed by five
   basis points — one of them looking good is unremarkable.
3. **The choice of rules was informed by seeing this market.** Not by fitting,
   but not by ignorance either.

## Verdict: all three REJECTED (2026-09-09)

Run over 2013-01-01 to 2026-09-08 **after** the corporate-action repair described
below. The numbers from the first attempt are superseded and are kept in the
history rather than deleted.

| rule | net CAGR | excess vs Nifty 500 | max drawdown | turnover |
|---|---|---|---|---|
| **H3 low volatility, annual** | 9.84% | **−2.38%** | 16.5% | 0.9x/yr |
| **H4 short-term reversal, monthly** | −6.07% | **−18.28%** | 76.4% | 14.0x/yr |
| **H5 equal weight, annual (yardstick)** | 6.92% | **−5.29%** | 49.5% | 0.5x/yr |

Benchmark: Nifty 500 at 12.21% over the same period.

The registered bar required a positive excess after all three adjustments
together -- Nifty 500, minus 1.2 points for the price index, at 10 bps of
slippage. Nothing here is positive before any of them. **All three are
rejected.**

### What each one says

**H3 is the interesting failure.** Low volatility beat the yardstick by 2.9
points and did it with a third of the drawdown -- 16.5% against 49.5%. The
signal works as a signal: sorting on volatility genuinely picked better names
than not sorting at all. It still lost to the index by 2.4 points, so it is
rejected, but it is the only rule in this repository whose signal has
demonstrably contributed anything.

**H4 is the clean confirmation of the cost thesis.** Buying last month's losers
turned over 14 times a year, paid for it, and lost 18 points to the index with a
76% drawdown. The per-trade edge was approximately zero and the costs did the
rest. This is the outcome the whole cost model was built to be able to measure,
and it measured it.

**H5, the yardstick, is the finding that matters most and it is not a result
about strategies at all.** Holding fifty of the most-traded Indian stocks equal
weighted should roughly track a broad index. It lags by 5.3 points, and **that
gap is not yet explained.** Until it is, every number in this table and in
experiments 001 and 002 carries an error bar of that size. Candidate causes,
none confirmed: the 681 entities with no adjusted series, whose corporate
actions remain unrecoverable; the residual ~10% cash; and the genuine drag of
equal-weighting a turnover-ranked universe that included RCOM, UNITECH,
JPASSOCIAT and other eventual zeros while rebalancing into them as they fell.

### Two defects these rules exposed

**The universe contained fund units.** NSE lists ETFs in the same cash segment as
shares with the same series, so 361 of 2,884 tradeable entities were funds. "Hold
the twenty least volatile names" selected six money-market ETFs and some gold: at
the first attempt H3 tested nothing about equities and returned 1.53%. Momentum
never noticed because a cash fund has no twelve-month momentum. Fixed by
filtering on the ISIN prefix.

**The rebalance leaked cash.** It traded the difference between target and
holding; the sell side always executed and the buy side was refused for falling
under the minimum notional, so one half of every rebalance completed. A symmetric
band fixed it.

### And the one that invalidated everything

Corporate actions were never applied to held positions. That is documented in
experiment 002's correction; it cost these three rules between 3 and 4 points
each, and it is why the first run of this experiment is superseded.

### Verdict

**All three rejected.** The low-volatility signal is the only one worth
remembering, and only as a direction: low volatility plus something else, at low
turnover, is the shape that has come closest.
