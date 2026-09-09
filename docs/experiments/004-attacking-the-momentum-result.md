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
