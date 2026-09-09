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

## CORRECTION (2026-09-09): the verdict below was computed with a bug

**Everything from here to the end of this file is superseded.** It is kept
rather than rewritten, because a record that quietly replaces a wrong answer
with a right one teaches nobody anything, and because the size of the error is
the most useful thing this experiment produced.

### The bug

**Corporate actions were never applied to held positions.** NSE's archive of
record prints the price actually traded, so a 1:1 bonus halves it overnight. A
real holder wakes with twice the shares and the same money; this backtest woke
with the same shares at half the price and had silently lost half that position.

Reliance did this in 2017 and again in 2024. HDFC Bank in 2025. Infosys three
times. HCL Tech twice, Bajaj Finance twice. Every name a liquid Indian momentum
book holds. There are 15 to 83 such events a year in liquid names, so a
twenty-name portfolio met several annually and each one destroyed roughly a
percent of it.

The design listed an `adjustments` table from the beginning and framed it as a
signal problem -- a return computed across a split reads -90%. That framing was
wrong about where the damage was. The signal fence was built and the book was
left broken, and the book is where the money is.

### The corrected result

| | published below | corrected |
|---|---|---|
| in-sample 2013–21 excess | +5.45% | **+9.23%** |
| **holdout 2022–26 excess** | **+0.78%** | **+9.14%** |
| holdout max drawdown | 34.8% | 31.4% |

Against the bar this experiment registered in advance -- Nifty 500, minus 1.2
points for the price index, at 10 bps of slippage per leg -- the corrected
holdout reads **+8.67%**. It clears. Drawdown is inside the 50% disqualifier.

**By its own pre-registered criteria, H2 is supported.** The verdict below is
withdrawn.

### Four reasons that is not yet a finding

1. **5.3 points are unexplained.** The equal-weight control in experiment 003 --
   fifty liquid stocks, no signal at all -- still lags the index by 5.3 points
   after this repair. A control that cannot track is a harness that is not
   understood, and every number here carries an error bar of that size until it
   is closed.
2. **Against the benchmark this file itself called fairest, the excess is
   marginal.** Versus the Nifty Midcap 100, closest to what the book actually
   holds, the corrected holdout excess is +2.09%, or about +0.9% after the
   dividend adjustment. The Nifty 500 was the registered primary and stays the
   primary; preferring it now that it flatters the result would be exactly the
   move this document was written to prevent.
3. **The holdout has now been read twice.** Once through a bug and once
   repaired. Re-running after a correctness fix is repair rather than a second
   attempt -- a number computed with a known defect was never evidence -- but the
   evidential value of this period is lower than a single clean reading, and
   pretending otherwise would be dishonest.
4. **This change turned a null into a strong positive**, which is the exact shape
   of motivated reasoning. What guards it: the direction and rough size (3 to 8
   points) were predicted in writing before any corrected number was produced;
   the twelve largest corrections are verifiable against public record; each
   action must be independently corroborated by the price move it caused, which
   rejected 183 of 892 candidates; and the fix moved the control 4 points toward
   the index rather than only moving the strategy. That is not sufficient. It
   needs someone hostile to it.

### Status

H2 is **not rejected and not established**. It is a corrected measurement with a
known unexplained gap, which is a different thing from a result, and nothing
should be traded on it.

---

## SUPERSEDED — Verdict: H2 NOT SUPPORTED (2026-09-08)

**The holdout is spent.** One run, recorded as `fde17ff7-3e4c-48c0-80db-8c72155e6ea6`
against snapshot `7a59dbf0…`. `verdict` will report any further reading of this
period as a repetition, and it is.

**Primary metric: +0.78% over the Nifty 500.** Positive by the letter of the
criterion above and **not an excess return** by the limitation declared beside
it, which said in advance: *a price benchmark understates the market by its
dividend yield, roughly 1.2% a year… if the measured excess return is smaller
than that, it is not an excess return.* 0.78% is smaller than 1.2%. Against a
total-return Nifty 500 this book returned roughly **−0.4%**.

| | in-sample 2013–21 | holdout 2022–26 |
|---|---|---|
| net CAGR | 18.99% | 10.19% |
| Nifty 500 | 13.53% | 9.41% |
| **excess (primary)** | **+5.45%** | **+0.78%** |
| max drawdown | 49.1% | 34.8% |
| turnover | 5.9x/yr | 7.4x/yr |

Everything pre-declared, run and reported:

| variant | excess |
|---|---|
| primary, 0 bps slippage | +0.78% |
| **10 bps slippage per leg** | **−1.44%** |
| **25 bps slippage per leg** | **−1.88%** |
| vs Nifty 50 | +3.58% |
| **vs Nifty Midcap 100** | **−6.27%** |

Drawdown was 34.8%, inside the 50% disqualifier. That is the only test it passed
and it is moot.

### Reading it honestly

**The in-sample estimate overstated by about 4.7 percentage points.** That is the
whole reason a holdout exists, and it is exactly what a holdout is supposed to
catch. Nothing was wrong with the in-sample run; a nine-year in-sample excess of
+5.45% simply did not survive contact with data the hypothesis had not seen.

**Ten basis points of slippage a leg turns it negative.** For a book turning over
7.4 times a year in names below the Nifty 50, ten basis points is a modest
assumption, not a pessimistic one. The strategy's survival depends on a number
nobody has measured, and the two plausible values either side of zero straddle
the answer.

**Against the fairest benchmark it loses by six points.** A top-500-by-turnover
momentum book is largely mid-cap. The Nifty Midcap 100 compounded at 16.46% over
this period; the strategy managed 10.19%. Measured against what it actually
holds rather than against the large-cap index, it did not beat buying the market
— it lost to it badly.

So the +0.78% headline is the most flattering framing available: zero slippage,
price benchmark, large-cap comparison. Change any one of those three and it goes
negative.

### What this means

**H2 is rejected, and the holdout is gone.** No variant of this rule gets another
reading of 2022–2026. Testing more ideas against data whose answer is now known
is how a null result gets converted into a false positive, and the whole point of
sealing it was to make that impossible rather than merely discouraged.

Both registered hypotheses are dead. Cross-sectional momentum on NSE, as
specified here, does not survive costs — which is what the reference project this
design started from found across 38 experiments, and what the null said all
along.

### What was worth building anyway

The result is believable, and that is the deliverable. A backtest that had
skipped the survivorship work would have shown a better number; one that had
guessed at charges would have been wrong by a paisa on every trade forever; one
that had matched holdings by ticker would have reported −3.77% instead of
−5.73% for H1 and never known why; and one without a sealed holdout would have
kept searching until something looked good.

Anything tested next needs a new registration, a new criterion, and **new data
to be tested against** — which means either a genuinely different hypothesis
tested on a fresh split, or waiting for time to pass.

### Verdict

**Rejected.**
