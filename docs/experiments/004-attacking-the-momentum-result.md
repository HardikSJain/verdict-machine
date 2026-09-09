# Experiment 004: attacking the momentum result

**Registered 2026-09-09, before any of these checks was run.**

Experiment 002's corrected holdout reads +8.27% excess over the Nifty 500. It
clears the bar that experiment registered in advance. It is also the product of a
change I made myself, to my own code, that turned a null into a strong positive
— which is the exact shape of motivated reasoning, and no amount of care on my
part substitutes for someone trying to break it.

Nobody hostile is available, so this is the next best thing: the attacks are
listed here **before** any is run, and every one is reported whatever it shows.
A robustness check that only gets mentioned when it passes is not a check.

## What would have to be true for +8.27% to be an illusion

Six ways, and each has a test.

**A1 — It is concentrated.** A single extraordinary year or a handful of months
can carry a whole CAGR. If the excess lives in one stretch it is one observation
wearing the clothes of fifty.
*Test:* year-by-year excess over the holdout and the in-sample period.

**A2 — It is a size premium wearing a momentum badge.** The universe sits below
the Nifty 50. Measured against what the book actually holds, the excess may be
close to nothing.
*Test:* excess against the Nifty Midcap 100 and the Nifty 50, alongside the
registered Nifty 500.

**A3 — It rests on instruments that are not equities.** The runs behind the
corrected number did not use `--equities-only`, so the universe still contains
361 fund units. A momentum rule should not select them, but "should not" is not
a measurement.
*Test:* the same run with fund units excluded.

**A4 — The unrecovered corporate actions flatter it.** Demergers cannot be
expressed by this layer. They are currently modelled as a price fall with no
compensation, which understates — but only if my reasoning about the direction
is right.
*Test:* count the unexplained drops in the momentum book and price the worst case
where each one destroyed the whole position.

**A5 — It dies under realistic friction.** Turnover is 7.4x a year and slippage
is still zero. This is the largest unmeasured risk in the entire project.
*Test:* 10, 25 and 50 basis points a leg, floor pinned so the risk gate does not
change underneath the comparison.

**A6 — Something in the code sees the future.** The adjustment layer is derived
from eod2, which is adjusted as of TODAY and therefore encodes every future
corporate action. If any signal path reads it, the result is worthless.
*Test:* audit every read the strategy makes for its source, and confirm the
adjusted series reaches the book only through an ex-date-gated application.

## What would sink it

Any of: the excess concentrated in under a quarter of the periods; a negative
excess against the Midcap 100 after the dividend adjustment; a sign change at 25
bps of slippage; or any signal path reading the adjusted series.

## What gets reported

All six, in this file, whatever they say — including the ones that pass, so the
ones that fail cannot be read as the whole story.

## Verdict

Not yet run.

---

## Verdict: the result does not survive (2026-09-09)

All six were run. Two things happened that the registration did not anticipate,
and both are reported before the scores because both change how to read them.

**The attacks found a bug, so every number in this file is from a third
measurement of the holdout.** A4 asked me to count unexplained drops in the
momentum book. There were three, and all three were traced rather than counted.
One of them, Ami Organics falling 51.4% in one session, was a 1:2 split the
adjustment layer had missed: Ami Organics reissued
its ISIN at the split, as NSE always does, and then renamed itself to Acutaas
Chemicals. Symbols are labelled by their latest ticker, so the pre-split
unadjusted bars read AMIORG and the whole adjusted series read ACUTAAS, and the
join that was supposed to span the ISIN seam never brought them together.

That is the exact mirror of the bug fixed the same morning. Joining by entity
missed actions across ISIN reissues the roster had not linked (Yes Bank).
Joining by ticker missed actions where the company also renamed. Both keys are
now emitted and the candidates unioned; 725 detected actions became 752. The
headline moved from +8.27% to **+9.14%**, and the in-sample figure moved the
other way, from +10.64% to +9.23%.

**So the holdout has now been read three times.** Once through a bug, once
through a partial repair, once through a fuller one. Each reading was a
correctness fix rather than a second attempt at the hypothesis, but that
distinction protects the logic, not the evidence: the evidential value of this
period keeps falling and there is nothing left to replace it with.

### The six

| | attack | registered sinking condition | result |
|---|---|---|---|
| **A1** | concentration | excess concentrated in under a quarter of the periods | **FAILS** |
| **A2** | size premium | negative vs Midcap 100 after the dividend adjustment | survives by 0.89 pts |
| **A3** | non-equities | — | **passes**, improves to +12.89% |
| **A4** | unrecovered actions | — | **passes**, worst case costs 0.94 pts |
| **A5** | friction | sign change at 25 bps | **passes**, positive to 100 bps |
| **A6** | look-ahead | any signal path reading the adjusted series | **passes**, none |

### A1 — concentration. Fails.

Holdout, year by year. 2022 is a full year and 2026 runs to 8 September.

| year | book | Nifty 500 | excess | Midcap 100 | excess |
|---|---|---|---|---|---|
| 2022 | −2.24% | +1.61% | −3.85 | +2.34% | −4.58 |
| 2023 | +57.52% | +25.76% | **+31.76** | +46.57% | +10.95 |
| 2024 | +30.66% | +15.16% | +15.50 | +23.86% | +6.80 |
| 2025 | +1.44% | +6.69% | −5.25 | +5.74% | −4.30 |
| 2026 | +8.57% | −3.00% | +11.57 | +3.81% | +4.76 |

Two of five periods are negative. Removing one year at a time, and recompounding
the rest:

| removed | excess vs Nifty 500 | excess vs Midcap 100 |
|---|---|---|
| nothing | +9.14 | +2.09 |
| **2023** | **+4.37** | **+0.33** |
| 2024 | +7.52 | +0.92 |
| 2025 | +13.46 | +4.10 |

One year in five carries 52% of the excess against the registered benchmark and
**84% of it against the benchmark closest to what the book holds**. One period of
five is a fifth, which is under the quarter this experiment wrote down in
advance.

The condition as registered was not sharp — "concentrated in" needed a threshold
I did not give it, and I am not going to supply one now that I can see the
number. So: against the Nifty 500 this is marginal and arguable. Against the
Midcap 100 it is not arguable. Take out 2023 and there is nothing left.

### A2 — size premium. Survives, barely, and only in isolation.

| benchmark | excess | after −1.2 pts for the price index |
|---|---|---|
| Nifty 50 | +11.94% | +10.74% |
| **Nifty 500** (registered primary) | **+9.14%** | +7.94% |
| **Nifty Midcap 100** (closest to the book) | **+2.09%** | **+0.89%** |

The registered condition was a *negative* excess against the Midcap 100 after
the dividend adjustment. +0.89% is not negative, so A2 does not sink it on its
own terms, and the Nifty 500 stays the primary because it was registered as the
primary — switching now that the Midcap is less flattering would be the move
this file exists to prevent.

But the spread across benchmarks is the finding. Nearly ten points of the excess
against the Nifty 50 is gone by the time the comparison reaches the size band
the book actually trades in. Whatever this rule is capturing, most of it is
being small.

### A1 and A2 together — and they were not registered together

Neither triggers on its own. Both at once do: drop 2023 and the Midcap excess is
+0.33% before the dividend adjustment, which is **−0.87% after it**.

I did not pre-register that conjunction, and combining two attacks after seeing
both results is a degree of freedom. It is reported as what it is: not a
registered test, and not evidence at the standard the rest of this file holds to.
It is the reason the verdict below is what it is, and a reader is entitled to
discount it accordingly.

### A3 — non-equities. Passes.

With fund units excluded the excess **rises** to +12.89% (net CAGR 22.30%). A
twelve-month momentum ranking never had a reason to select a money-market ETF,
and the measurement confirms it: the contamination that ruined the
low-volatility rule in experiment 003 is not doing any work here. The registered
runs stay the registered runs, so +9.14% remains the headline.

### A4 — unrecovered corporate actions. Passes, and its direction holds.

Two unexplained drops survive in the holdout book, priced at the equity of the
day each landed:

| date | name | fall | residual | share of book |
|---|---|---|---|---|
| 2024-12-20 | STAR | −54.1% | ₹22,516 | 2.27% |
| 2026-03-09 | CUPID | −77.2% | ₹11,725 | 1.23% |

The worst case — each was in truth a total loss rather than an action this
project cannot recover — costs 3.50% of the book, taking the excess from +9.14%
to **+8.20%**. That is the pessimistic bound, and the honest reading is that the
truth is on the other side of the measured number.

Both were traced. They are different failures, and neither is the one this
attack was registered expecting:

**STAR — a demerger, and the method's premise fails on it.** The whole detector
rests on eod2 being adjusted, so that the quotient of the two sources steps at
every action. For a demerger it does not step, because **eod2 does not adjust for
demergers either**: its own series carries 1485.05 on 5 December 2024 and 682.30
on the 20th, the same fall the unadjusted archive shows. The factor reads 1.0 on
both sides of the event. There is no candidate for the fraction filter or the
price check to reject — there is nothing there at all. (The stock was also
suspended for the ten sessions in between, which is why the book saw the whole
fall land in one mark.)

This is a limit of the method, not a bug in it, and it is worth stating plainly:
**this project can recover splits and bonuses and cannot recover demergers.** A
holder of a demerged company receives shares in a new listed entity, and no
ratio applied to the old one can represent that.

**CUPID — a real 1:5 split that the price check refused.** The factor steps from
5.0 to 1.0 cleanly and matches the fraction 5/1 exactly. It was then discarded
because the price fell 77.2% where a 1:5 split implies 80%: the stock moved about
14% in its own right on the ex-date, past the 10% corroboration tolerance.

That is the guard working as designed and paying its stated price. The tolerance
exists because 183 of 892 candidates had no corroborating price move at all, and
loosening it to admit CUPID would readmit those. **A false negative that costs
one position is the cheap error; a false positive multiplies a share count and
invents money.** It is recorded here as a measured cost of that choice rather
than a reason to revisit it.

Both understate the book rather than flatter it, which is the direction A4
predicted in advance.

### A5 — friction. Passes the registered condition, and exposes something else.

| slippage per leg | excess |
|---|---|
| 0 bps | +9.14% |
| 10 bps | **+9.87%** |
| 25 bps | +7.12% |
| 50 bps | +6.22% |
| 75 bps | +3.57% |
| 100 bps | +1.61% |

No sign change at 25 bps; the fitted crossing is near 120 bps. For a book turning
over 7.4 times a year this is a real improvement on experiment 002, which went
negative at 10 bps before the corporate actions were repaired.

**But look at the 10 bps row: adding friction improved the result by 0.73
points.** That cannot happen in the cost model, and it does not — slippage is
charged against the fill on both sides and the sign is right. What it means is
that a 10 bps perturbation changes which orders clear the minimum-notional
floor, the book takes a different path, and 4.7 years later the answer has moved
by three quarters of a point in the wrong direction.

**So the estimator's own noise band is roughly ±1 point.** That is the number to
hold against everything else in this file. It is smaller than the +9.14% against
the Nifty 500. It is *larger than the +0.89% against the Midcap 100 after the
dividend adjustment.* Against the benchmark closest to what this book holds, the
measured edge is inside the measurement error.

### A6 — look-ahead. Passes.

The adjustment layer is derived from eod2, which is adjusted as of today and
therefore encodes every future corporate action. Audited: no strategy, engine or
report path names eod2. The only reads of the `adjustments` table are
`AdjustmentsOn`, whose query filters `ex_date = $1` and can see nothing beyond
the session being simulated, and the insert-time version check. The adjusted
series reaches the book only through an ex-date-gated application.

## Verdict

**H2 is not established, and I am no longer treating the corrected result as a
positive finding.**

It clears five of its six registered conditions. That is not the same as being
real, and here is the whole of it in one line: **against the benchmark closest to
what this book actually holds, after the dividend adjustment the excess is +0.89
points, and the measurement's own noise is about a point.** Take out its best
year and it is negative. What is left is a mid-cap size premium and 2023.

The three attacks it passed cleanly are worth keeping and are not consolation.
A3 and A6 rule out two ways the number could have been an artefact. A5 says the
rule is no longer killed by a plausible friction assumption, which is a genuine
change from experiment 002. None of that makes the remainder a finding.

**Nothing is traded on this.** The holdout is spent three times over and no
further reading of 2022–2026 is evidence about momentum.

### What this experiment actually produced

Not a result about momentum. Two things about the machine:

1. **A bug that three prior readings of the same period did not surface**, found
   by pricing an anomaly the report had been printing all along and nobody had
   made numeric. `DetectAdjustments` — the function behind the largest
   correction in this project — had no test at all, and its join was rewritten
   twice in one day with nothing noticing. It has three now.
2. **A measured error bar.** Until A5 the estimate had no stated precision. It
   has one now, about ±1 point, and it is large enough to swallow the result
   whenever the fair benchmark is used.

### One thing I did wrong while doing this

Updating a summary table, I ran the **filtered** rule — experiment 001's H1 —
over 2022–2026 to refresh a number. Experiment 001's kill criterion fired
in-sample and it says, in writing, that its holdout was never spent. It has been
now, by me, casually, to fill in a cell.

The reading was +4.70% excess. It is recorded here so that it is on the record
rather than discarded, and experiment 001 has been corrected to say its holdout
is spent. The damage is small — experiment 002 had already spent this period
three times for the same underlying rule with the filter off — but the mistake
is not the damage. A discipline that only holds while it is convenient is not a
discipline, and the failure mode was exactly the one the whole apparatus exists
to prevent: reading sealed data without deciding to.

### Limitations of this review

- Six attacks are not all the attacks. The ones not run include: a
  purged/embargoed re-estimate, a bootstrap over the trade distribution, and any
  test of whether the universe construction itself leaks.
- The A1-and-A2 conjunction was not pre-registered and is reported as unregistered.
- **A4 changed what it was measuring while it ran.** It was registered to price
  the worst case; it ended up tracing causes, and the trace is what produced the
  demerger limit and the corroboration cost. Both are worth having and neither
  was the registered test.
- The adversary is the same person who wrote the code and wants it to work.
