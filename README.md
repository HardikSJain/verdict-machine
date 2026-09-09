# Verdict Machine

A research system for testing whether a stock-trading rule actually works, built
for the Indian market (NSE).

It was built to answer one question about one strategy. **The answer was no**,
and the interesting part is why that answer can be believed.

Nothing here is financial advice, and nothing here is running with money.

---

## The result

Two hypotheses were registered in advance, with their kill criteria and their
limitations written down **before** any number existed. Both were rejected.

| | in-sample 2013–21 | holdout 2022–26 |
|---|---|---|
| 12-1 momentum **with** a 200-day trend filter | **−5.73%** vs Nifty 50 | never run — killed in-sample |
| 12-1 momentum **without** the filter | +5.45% vs Nifty 500 | **+0.78%** |

That +0.78% is not an excess return, by a limitation declared before the run: the
benchmark is a price index, it excludes roughly 1.2% a year of dividends, and an
excess smaller than that is not an excess. Against a total-return benchmark the
strategy returned about **−0.4%**. Ten basis points of slippage a leg — modest
for a book turning over 7.4 times a year — takes it to **−1.44%**. Against the
Nifty Midcap 100, which is closer to what the book actually holds, it loses by
**6.27%**.

The full reasoning, including everything declared before the result, is in
[docs/experiments](docs/experiments/).

**The holdout is spent.** 2022–2026 got exactly one reading, it is recorded in
the `runs` table, and any further reading is reported as the repetition it is.

## Why the null is believable

Getting a negative result is easy. Getting one you can trust is the work.

**No survivorship bias.** The convenient data source silently drops companies
that died. Ask this system for the top 500 stocks of June 2015 and it returns Jet
Airways and DHFL — both since collapsed, both absent from that source. It ingests
NSE's raw archives alongside it for exactly this reason.

**Costs come from a real contract note, not a rate card.** Reproducing one to the
paisa found that GST is computed once at 18% and rounded once, while the CGST and
SGST halves printed on the note are a display split rounded independently — they
sum to a paisa *more* than what was debited. No rate card says this. A model built
from published rates is wrong on roughly half of all trades, forever, and nothing
without a real document would ever catch it.

**Splits cannot become returns.** NSE reissues a company's ISIN when it splits, so
Tata Steel's history breaks in two in July 2022. After repairing that, the series
is continuous in *identity* and still discontinuous in *level* — a momentum signal
crossing the split reads −90% and looks like a perfectly good number. Every return
goes through a fence that refuses such a window. Measured across all 444 links,
the price break lands one session *before* the ISIN change 262 times, so the fence
carries a guard band rather than testing the boundary alone.

**Orders fill at the next open, never the close they were decided on.** Enforced
by the loop, not by convention.

**The result is reproducible.** Every run records a `snapshot_id` hashing every
versioned table at its own ingest pin, a `config_hash` including the entity
roster actually applied, and the git commit. A replay that reproduces all three
and gets a different number has found a bug.

## Four bugs that would have made the answer wrong

Each was found by running against real data, and each would have flattered or
distorted the result silently.

**Holdings were matched to prices by ticker.** Tickers change while you hold them
— RNAM became NAM-INDIA, ADANIGAS became ATGL, IBSEC became IBVENTURES became
DHANI, all under one unchanged ISIN. A renamed holding became unsellable, stuck
in the book forever at its last known price, inflating equity. This repository
spent its first milestone building a careful identity system and then keyed its
engine on a display label. Fixing it moved the first result from −3.77% to
−5.73%.

**Positions were marked at their purchase price after a halt.** A name bought at
100 and trading at 120 for six months would value at 100 again for any session it
did not trade, printing a drawdown and a recovery around a day on which nothing
happened.

**NSE's own files are inconsistent.** One 2020 bhavcopy writes the year with two
digits where fourteen years of others use four. The index archive writes
`MM-DD-YYYY` for a few April 2023 sessions and `DD-MM-YYYY` for the rest of the
same week — and `04-10-2023` is 10 April one way and 4 October the other, which
is a real trading day. A parser that simply picked a layout would have filed
April's data onto October's date silently. The filename is the authority: a date
format is accepted only if it reproduces the session requested.

**The backfill skipped weekends.** NSE trades some Saturdays — Budget days,
Diwali Muhurat — and 19 real sessions were missing before anyone noticed.

## Architecture

A Go modular monolith over Postgres. Everything that differs between a backtest
and a live evening sits behind a seam, so the same loop runs both.

```
cmd/verdict/          one binary: migrate, ingest, backfill, universe, entities, index, backtest
internal/market/      insert-only versioned store, point-in-time universe, the succession fence
        /bhavcopy     NSE equity archives (the source of record)
        /eod2         adjusted daily CSVs (the cross-check)
        /nseindex     NSE daily index archive
        /entities     canonical-entity roster, its gates, and the apply path
internal/cost/        dated charge schedule; every rate carries its source and whether a
                      document reconciles it
internal/risk/        RiskGate: caps, kill switch, and a position floor derived from the charges
internal/engine/      the loop, next-open fills, paper broker, portfolio
internal/strategy/    registered strategies and market filters
internal/report/      the published figures, with their caveats printed beside them
```

**Nothing is stored that cannot be replayed.** `bars`, `symbols`, `symbol_links`,
`index_levels` and `runs` are insert-only, enforced by row triggers. A correction
is a new row. Every read is pinned to an `ingested_at`, so a query asked today
about 2015 returns what was knowable then — including what was wrong then.

**Identity is an overlay, never a rewrite.** 444 links across 415 entities map
physical symbols to canonical companies. The price rows were never touched; a bad
link is undone by writing one more row.

**Refusing beats guessing.** A return spanning a split is refused. A charge rate
nobody has reconciled is used but reported. A market filter that cannot read its
index holds the book rather than liquidating it. An index rename is merged only
after the level is *measured* continuous across it.

## Data

13.8 million daily bars and 292,841 index levels. NSE's raw archives run from
September 2011 and the index archive from February 2012; the adjusted source
reaches back to 1995 but is survivor-only and is used as a cross-check, not as
the source of record.

The index series is verified against data it has no connection to: Nifty 50 daily
returns correlate **0.9681** with the NIFTYBEES ETF across 3,600 sessions —
different file, different loader, different identity scheme, and nothing in the
code forcing them to agree. Spot closes match documented values exactly on the
COVID low, the 2021 peak, election day and demonetisation.

```bash
make db-up && make migrate
export VERDICT_DATABASE_URL=postgres://verdict:verdict@localhost:5433/verdict?sslmode=disable

bin/verdict backfill --from 2011-09-01     # NSE equity archives
bin/verdict index backfill                 # NSE index archives
bin/verdict universe --as-of 2015-06-30    # the survivorship proof
bin/verdict entities check                 # monthly identity audit
bin/verdict index check                    # every index rename, level-verified
bin/verdict backtest                       # in-sample; refuses to read the holdout
```

The adjusted cross-check source is produced by
[eod2](https://github.com/BennyThadikaran/eod2) (GPL-3.0), run as a separate
process by `scripts/eod2-sync.sh`; this repository only reads its CSVs.

## Tests

```bash
make test          # unit + integration, needs make db-up
make test-short    # unit only, no database
```

232 tests. The ones worth reading are the ones that assert something
uncomfortable: that a wrong-but-disjoint entity merge is undetectable from inside
the store, that the succession fence's protection is contingent on the caller's
pin and is not retroactive, and that a renamed holding can still be sold.

Claims are verified by breaking them. The guard band, the entity map's pin, the
GST rounding rule and the never-block-an-exit principle each have a mutation
recorded against them: change the code and a named test fails with the wrong
number in its message.

CI runs `gofmt`, `go vet`, the full suite against a real Postgres, and a **golden
backtest** — the whole stack replayed over a frozen slice of the archive, with
every published figure compared to a committed expectation. A one basis point
change to STT fails the build.

## Status

Milestones 0 and 1 are complete: the data foundation, and a first honest verdict.

Milestones 2 through 5 — a hash-chained ledger, an evening alert pipeline, a
supervised alert phase, and limited autonomy — were designed to *trade* a
strategy. There is no strategy. Building them now would be building a machine to
lose to the index carefully, so they are not built.

What would change that is a new hypothesis, registered in advance and tested
against data it has not seen. The holdout regenerates at one month per month.

## Reading further

- [docs/DESIGN.md](docs/DESIGN.md) — the full design, every decision and its
  reason, including the ones that turned out wrong and what replaced them
- [docs/experiments/](docs/experiments/) — both registered hypotheses, their
  pre-declared limitations, and their verdicts

MIT.
