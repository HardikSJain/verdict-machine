# Experiment 005: how much of a result is the calendar?

**Not a hypothesis about a market. A measurement of this instrument.**

Every number this project has published came from one backtest: start on the
first session of the period, hold twenty names, rebalance on the last session of
each month. The month end was never chosen. It is what a person writes when they
need a rebalance date, and it has been carried unexamined through four
experiments.

Experiment 004 found it by accident. Attack A5 perturbed slippage and the excess
moved NON-MONOTONICALLY -- 10 bps read *higher* than 0 bps -- which cannot
happen in a cost model and does not. What it exposed is that the risk gate
refuses any trade below a hard notional floor, so a fill price a few paise
different pushes one order across that line and the book takes a different path
for the next four years. A continuous system with a cliff in it.

If a five-basis-point nudge does that, so does moving the rebalance by a day.

## The measurement

Rebalance on the last session of the month, then 1, 2, ... 20 sessions later.
Twenty-one runs. Same rule, same data, same costs, same universe, same
everything except which day the orders go in.

`--rebalance-offset` shifts in SESSIONS, not days, for the same reason the month
end is read off the archive rather than computed: a calendar offset lands on
holidays and drifts. Offset 0 is byte-identical to every published result, which
a test pins.

## Result: the spread is larger than the claimed edge

Experiment 002's rule, holdout 2022-01-01..2026-09-08, excess over the Nifty 500:

| | |
|---|---|
| **published (offset 0, month end)** | **+9.14%** |
| mean of 21 offsets | **+7.55%** |
| median | +7.09% |
| standard deviation | **±2.67 points** |
| minimum (offset 6) | +4.08% |
| maximum (offset 20) | +14.45% |
| **range** | **10.37 points** |

The published figure sits at the 71st percentile of its own distribution. It was
not wrong and it was not cherry-picked; it was one draw, reported to two
decimals, from a distribution nobody had looked at.

**A rule whose claimed edge is 9 points has a calendar spread of 10.**

## The same sweep against the fair benchmark, after the fence repair

Repeated with the succession fence repaired (it now prices returns across
explained corporate actions instead of refusing them), against both benchmarks:

| | vs Nifty 500 | vs **Nifty Midcap 100** |
|---|---|---|
| mean of 21 offsets | +8.16% | **+1.11%** |
| median | +7.46% | +0.41% |
| standard deviation | ±2.50 | ±2.50 |
| min / max | +4.96 / +14.88 | **−2.09 / +7.83** |
| **after the −1.2pt dividend adjustment** | +6.96% | **−0.09%** |
| offsets negative after dividends | 0 of 21 | **12 of 21** |

**Against the benchmark closest to what the book holds, the ensemble mean is
−0.09%: zero, with more than half the calendars negative.** Experiment 004
called this rule not established from a single reading of +0.89%. Twenty-one
readings say the same thing with an error bar, which is the difference between
a suspicion and a measurement.

The fence repair moved the Nifty 500 ensemble by +0.61 points, a quarter of one
standard deviation. By the rule this experiment sets, that is not a change and
gets no commentary.

## A near-miss worth recording

Before the sweep ran I predicted in writing that the Midcap ensemble would come
back near zero and negative after dividends. Three offsets in, the readings were
+3.63, +4.81 and +2.00 -- all comfortably positive -- and I said the evidence
was running against my own call.

Those were the three best offsets in the whole distribution. The other eighteen
average +0.66%.

Flagging the uncertainty was right. The near-miss is the lesson: at ±2.5, three
samples can point confidently the wrong way, and the only thing that prevented a
confident retraction of a correct conclusion was waiting for the run to finish.
It is the same error as narrating a 0.9-point move from a bug fix, arrived at
faster.

## What this invalidates, and it is not the bug fixes

Three corporate-action defects were found and fixed on 2026-09-09. The headline
moved +0.78% -> +8.27% -> +9.14% and each move was written up carefully;
experiment 002 carries a table tracking how the detector's join changed the
answer by 0.9 points.

**Every one of those movements is smaller than one standard deviation of this
distribution.** The fixes are still right -- a book that loses half a position to
a missed bonus is broken, and the tests prove the arithmetic independently of any
backtest. What was never measurable is the CLAIM ABOUT THE EFFECT. The honest
sentence was always "a real defect is fixed; its effect on the estimate is below
what this method can resolve," and instead four documents said the result
improved.

That is the finding here. Not that the machine is noisy -- that the reports were
written as though it were not.

## What changes from now on

1. **A single backtest is not a result.** It is one sample. Anything reported as
   a finding is the ensemble across offsets, given as mean and spread.
2. **Differences smaller than the spread are not differences.** No commentary on
   a change of a point or two, in either direction, without the distribution
   under it.
3. **Fixes are justified by their arithmetic, not by the backtest moving.** A
   corporate action either was or was not applied to a held position; that is a
   question with a right answer and a unit test, and it does not need a P&L
   number to be worth fixing.

The averaged ensemble is also, separately, a better portfolio: staggering entries
across the month is what a real book does, and it is the version of this rule
that a person could actually run.

## Limitations

- Twenty-one overlapping paths are not twenty-one independent samples. They
  share holdings, so the standard error of the mean is larger than sd/sqrt(21).
  The SPREAD is the honest headline, not a confidence interval.
- Only the rebalance calendar was varied. Start date and starting capital are
  two more arbitrary choices with the same cliff behind them, and neither has
  been swept.
- This measures ONE rule over ONE period. There is no reason to think ±2.7
  points transfers to a different cadence -- a rule trading once a year has far
  fewer chances to fall off the notional cliff.
