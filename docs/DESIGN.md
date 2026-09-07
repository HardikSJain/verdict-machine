# Design: Verdict Machine — a daily-bar NSE strategy lab that earns autonomy

Repo: HardikSJain/verdict-machine
Branch: main
Status: APPROVED

## Problem Statement

Build a personal algorithmic trading system for NSE equities and ETFs that (1) backtests a
strategy honestly enough that a negative result is believed, (2) runs an alerts-only phase
where it proposes orders every evening and the human confirms each one in Kite, and (3)
earns limited autonomy to place orders itself only when it has proven it behaves exactly
as the backtest said it would. Personal use, but built to portfolio grade: strong
architecture, scalable along the axes a hiring engineer will ask about, and a public
artifact at the end.

The builder's own words: "although this is a personal use only, i wish to think very
strong architecture and a super solid portfolio project. make sure to make it highly
scalable whenever we design the architecture." And on horizon: "Lets start with
Daily/weekly NSE stocks, ETFs. maybe if possible and successful then maybe intraday as an
extension."

## What Makes This Cool

- **The system is a verdict machine, not a money machine.** The deliverable is an honest
  answer, whichever way it goes. A net-negative strategy proven net-negative with full
  provenance is a valid win, and the README is written around that.
- **A publicly verifiable forward test.** Every evening the proposed order set is hashed
  and the hash is pushed to a public repo before the next open; the plaintext is revealed
  after the close. The ledger is hash-chained, so skipped trades and overrides are
  tamper-evident even to the builder. After a year this is "here is a verifiable forward
  test, including what I skipped and why", which almost nobody has.
- **Autonomy is earned, and the rule is written down before the first alert.** Not "when I
  feel good about it". The gate measures operational fidelity, and P&L can only kill.
- **One engine, two clocks.** The same code that ran the backtest runs the evening. Replay
  regenerates any past evening byte-for-byte from its data snapshot and git sha.

## Constraints

- Horizon: daily and weekly bars, delivery (CNC) trades, NSE equities and ETFs. Intraday
  is a later extension; the clock and bar abstractions allow it, v1 builds no tick plumbing.
- Capital: the algo trades only the builder's "play" tier (at most 15% of net worth). It
  never touches the core SIP. Single stock stays under 10% of net worth.
- Regulation and broker, verified 2026-09-06: Kite Connect execution APIs are free on the
  personal plan; live and historical data cost ₹500/month; API order placement needs a
  static IP whitelisted with Zerodha (up to two, changeable weekly); 10 orders/sec cap;
  Kite Publisher basket orders, where the user confirms in Kite, sit outside SEBI's retail
  algo framework. Orders placed via baskets outside market hours become AMOs automatically.
- Data: free NSE UDiFF bhavcopy via the eod2 project (GPL-3.0, active, pushed 2026-08-31)
  run as a separate producer process. eod2 code is never linked into the Go binary.
- Language: Go, because it is on the builder's 2027 skills plan and a single binary on a
  systemd timer is the entire deployment story. Python stays read-only for research
  notebooks against the same Postgres.
- Engine size cap: ~500 lines. If it grows past that, the write-your-own decision is
  revisited against NautilusTrader.
- Every seam ships with two concrete implementations in v1 or it is not written.
- Public repo hygiene: no credentials, no personal capital figures, no broker client ids.
- Secrets: Kite api_key and secret, the daily access token, the Telegram token, and the
  Postgres DSN load from a 0600 env file; never in config, never in the ledger.
- Kite access tokens expire daily and scripted login breaches Zerodha's terms. The human
  completes one Kite login each evening in every phase; automation removes per-order
  confirmation, never the daily login. "Unattended" throughout this document means: no
  manual step other than the Kite login and, in phase 1, accept or skip.

## Premises (agreed 2026-09-07; premise 3 revised after cross-model review)

1. Daily/weekly bars, NSE equities and ETFs, delivery trades. Intraday later via the
   clock and bar abstractions; no tick plumbing in v1.
2. Honest measurement is the product: one engine for backtest and live behind a pluggable
   clock; point-in-time universe; corporate-action adjustment applied at read time as of a
   date; the full Indian cost model inside the backtest; a chronological holdout frozen in
   config before any tuning; provenance (git sha, config hash, data snapshot id) on every
   run. Discovery backtests stop at `holdout_start`; the historical holdout is run once per
   registered hypothesis; the alert phase is a second, live, out-of-sample period. A negative
   result is a result; the engine is never tuned until the curve turns green.
3. **(revised)** The alert phase is the live out-of-sample period with a human in the loop. Every evening
   signal is logged with accept, skip, and eventual fill. **Autonomy is earned on
   operational fidelity, not P&L:** live evening output matches the shadow replay ≥ 99% over
   ≥ 120 intents spread across ≥ 8 intent-bearing evenings, realised slippage within a
   configured bps band of the model on ≥ 40 fills, zero risk-gate breaches, and N consecutive
   intent-bearing evenings where the human accepted every order. Fidelity = share of live
   intents present identically in the replayed set. A monthly-rebalance book produces
   intents on roughly 12 to 18 evenings a year, so these clocks are sized in intents, not
   evenings; evenings with no intents count toward nothing.
   P&L is a **kill** criterion only: drawdown cap and tracking error versus the shadow paper
   book. Reason: at realistic per-trade dispersion (6 to 10%), 40 trades cannot distinguish
   a real edge from noise, so a P&L promotion gate either never opens or opens on luck.
4. Execution ladder is regulation-aware. Phase 1 alerts via Kite Publisher basket links
   (manual confirm, no static IP, free). Phase 2 autonomy via the Kite Connect execution
   API from a VPS with a whitelisted static IP. In both phases the human does the daily
   Kite login; phase 2 removes per-order confirmation only. Both phases pass every order
   through the same risk gate: kill switch, daily loss cap, single-stock cap, algo-book cap,
   max positions, and a minimum position size.
5. Capital: play tier only; never the core SIP; single stock under 10% of net worth.
6. Scalable means the right seams, not infra theatre: modular monolith; append-only
   hash-chained event and decision ledger in Postgres; strategies as registered plugins;
   broker, data, clock, and notifier as adapters; account_id on every account-scoped row
   (runs, events, decisions) from day one, market data shared;
   containerised Postgres; CI runs the backtest as a golden test. No microservices or
   Kubernetes in v1. The scale story is many strategies, many symbols, multi-account-ready,
   and intraday-ready by abstraction.
7. Data: the raw NSE bhavcopy archive is canonical, because it is the only one of the two
   sources that still contains delisted names and so the only one a point-in-time universe
   can be built from; eod2's adjusted series is the second implementation and the
   split-continuity cross-check, not the source of record. This inverts the premise as
   first written ("free NSE bhavcopy via eod2 is canonical"): see the resolved note under
   Open Questions. Kite history (₹500/month) is an optional cross-check, not a dependency.

## Cross-Model Perspective

Codex was unavailable (its configured model is not supported on a ChatGPT-account login),
so a fresh-context Claude subagent ran the cold read from a structured summary only.

- **Coolest version not considered:** the commit-reveal forward test and hash-chained
  ledger described above, plus `replay --date` that regenerates any evening byte-for-byte.
  Roughly 100 lines. Adopted into the recommended approach.
- **The tell:** the architecture excites the builder, not the alpha; "maybe if possible and
  successful then maybe intraday" shows he already expects a thin edge. Therefore the
  likely outcome (net-negative after costs) must be a valid win, and every interface must
  have two real implementations in v1 to guard against seam theatre. Adopted.
- **50% open source:** eod2 (BennyThadikaran) already provides adjusted NSE daily OHLCV and
  delivery data for 2000+ symbols since 1995, ISIN tracking across renames, and a holiday
  calendar. Run it as a cron producer, ingest its CSVs; the main system's language is
  irrelevant to it. Point-in-time index membership is the gap eod2 does not fill.
- **Weekend build:** Go, single binary on cron, layout `cmd/verdict` plus
  `internal/{market,cost,engine,ledger,broker,strategy}`; first three modules market, cost
  (with a golden test reproducing one real contract note to the paisa), then engine plus
  one deliberately dumb strategy. Skip Telegram, Kite Connect, dashboard, CI, and any
  intraday abstraction in the first weekend. Adopted as milestones M0 to M1.
- **Challenged premise:** premise 3's promotion rule was statistically empty. Accepted and
  revised (see Premises).
- **Challenge to the advisor:** write-your-own-engine is right only while the engine stays
  under ~500 lines. Adopted as a constraint.

## Approaches Considered

### Approach A: Weekend verdict in Python (minimal viable) — rejected
Python, eod2 as producer, polars, a ~300-line engine, SQLite, a Telegram script, cron on a
Mac. Fastest to a first edge check. Rejected because nothing in it reads as architecture,
and SQLite plus a script is what gets rewritten the moment phase 2 needs a VPS and a ledger
worth trusting. Its one idea survives: a Python research notebook can read the Postgres
store at any time.

### Approach B: Verdict machine in Go (ideal architecture) — chosen
See Recommended Approach.

### Approach B2: Same design in TypeScript — rejected
Identical architecture in Node, lifting TradeLabs' cost engine and charge model directly,
with kiteconnectjs and a later Next.js dashboard. Rejected because a second Node repo
beside a Next.js portfolio site signals less for a backend job switch than a Go service,
and the deployment story is weaker than a single binary. TradeLabs' charge schedule and
point-in-time universe approach are still borrowed, ported to Go.

### Approach C: Stand on NautilusTrader — rejected
Python strategy on Nautilus' Rust engine, with an NSE data loader and a Kite execution
adapter contributed upstream. Backtest equals live by construction. Rejected because it is
a venue-adapter project for a strategy that fires twice a month, and the scalable-design
story would be theirs rather than the builder's. Revisited only if the engine breaches the
500-line cap.

## Recommended Approach

Working name: **verdict machine**; binary `verdict`. Its own repository, MIT, public from the
first commit, with the forward-test hashes in a second public repo.

### Runtime shape

One Go binary, one Postgres. Subcommands: `ingest`, `backtest`, `evening`, `replay`,
`verify`, `tick`, later `evening --execute`. There is no HTTP server and no long-running
process in v1. A systemd timer on the VPS runs `tick` every 5 minutes from 19:00 to 23:30
IST on trading days; each `tick` (a) runs `ingest` if T's bars are missing, (b) runs
`evening` once per T, guarded by a `runs` row, (c) drains Telegram `getUpdates` from a
`telegram_offset` row, appending `intent.accepted` and `intent.skipped` events, and (d) on
a `/token <value>` message exchanges the token, reconciles, and exits without persisting
the token. Retries for a late bhavcopy fall out of (a) for free.

### Package layout

```
cmd/verdict/                  subcommands, config loading, config hash
internal/market/           bars, read-time adjustment as-of date, PIT universe, holidays
internal/cost/             dated statutory charge schedule, slippage model
internal/engine/           Clock, Bar, Strategy, Portfolio, next-open fills (≤ ~500 lines)
internal/strategy/         registered strategies; two shipped in v1
internal/risk/             RiskGate: kill switch, caps, daily loss, max positions
internal/ledger/           append-only events, prev_hash chain, provenance stamps
internal/broker/           Broker: paper, publisher (basket), kite (phase 2)
internal/gate/             promotion gate (fidelity) and kill criteria (P&L)
internal/forward/          commit-reveal: canonical JSON, sha256, push hash / reveal
internal/notify/           Notifier: telegram, stdout; accept/skip capture by polling
internal/report/           backtest and weekly reports (net vs gross vs benchmark)
migrations/                goose; sqlc for typed queries; pgx driver
research/                  Python notebooks, read-only against Postgres
testdata/                  small frozen data fixture for the CI golden backtest
```

### Seams and their two v1 implementations

| Seam | Implementation 1 | Implementation 2 |
|---|---|---|
| Clock | SimClock (steps over dates) | WallClock (IST, holiday-aware) |
| Data | eod2 CSV loader | `testdata/` fixture loader, which the CI golden backtest needs anyway |
| Broker | Paper (next-open fill, modelled cost) | Publisher (basket link, AMO) |
| Notifier | Telegram | stdout |
| Strategy | 12-1 cross-sectional momentum: top 20 by momentum from the liquid PIT universe, equal weight, all algo capital, rebalanced at the open of the first trading day of each month on the prior close's signals; when Nifty 50 closes below its 200-DMA the book liquidates to cash and stays in cash until it closes back above | 200-DMA trend on a small ETF set (dumb control), paper-only, no capital |

Kite Connect execution is the third Broker implementation and arrives in phase 2.

### RiskGate v1 parameters

- `max_positions = 20`. The DP charge per scrip per sell day dominates small lots, so a
  50-name book inside the play tier would be single-share lots paying more in DP charges
  than it can earn.
- `min_position_notional` such that the modelled round-trip cost (STT, exchange, SEBI,
  stamp, GST, DP charge, slippage) stays under 0.5% of notional; roughly ₹10k at current
  charges. Intents below it are rejected with a `risk.rejected` event, not rounded up.
- Kill switch is a `kill_switch(account_id, set_at, reason)` row read before every order,
  so the binary stays stateless; a local file flag is the override for when Postgres itself
  is the problem. Daily loss cap, single-stock cap and algo-book cap are rupee and
  percentage limits in config, hashed into `config_hash`.

### Data model (Postgres)

- All reference tables are insert-only with `ingested_at timestamptz not null`; a change is
  a new row and reads take the latest version of the key as of a timestamp.
- `symbols(symbol_id, isin, ticker, valid_from, ingested_at)` version key `isin`, with
  `symbol_id` stable across ticker renames. `ticker` is a scalar column, not the
  `ticker_history jsonb` this document first specified: the history is the version rows
  themselves, which is what the insert-only rule already gives. `valid_from` is the session
  date the ticker was observed for, so a read can ask "what was this called on this date"
  (market time) independently of "what did we believe then" (`ingested_at`, audit time).
- `bars(symbol_id, date, open, high, low, close, volume, delivery_qty, source, ingested_at)`
  version key `(symbol_id, date, source)`; the run config names the source; a revised
  bhavcopy adds a row, never updates one.

  Prices are stored **as the source reports them**, and `source` names the price
  convention: `nse-bhavcopy` is unadjusted, `eod2` is split/bonus adjusted in place. This
  document first said `bars` was unadjusted throughout; that is true of only one of the two
  sources, and one table now holds both conventions side by side. A CHECK constraint on
  `bars.source` keeps the allow-list in the schema rather than in comments.
- `adjustments(symbol_id, ex_date, factor, kind, ingested_at)` insert-only, applied at read
  time as-of a date, so a backfill fetched later can never double-adjust (the TradeLabs bug,
  2026-07-24). **It applies to unadjusted sources only** -- today that means
  `source = 'nse-bhavcopy'`. Applying it to the eod2 rows would double-adjust them, which
  is the exact bug this layer exists to prevent.
- `index_membership(index, as_of_date, symbol_id)` monthly snapshots, an upgrade path only
- Default PIT universe: top 500 by 6-month median rupee turnover, computed from `bars`
  alone as of each date, so it needs no external membership data and never looks ahead.
  Nifty 500 membership history is an upgrade if NSE's constituent archives cover the range.
- `holidays(date, exchange, ingested_at)` version key `(date, exchange)`
- `charge_schedule(effective_from, product, component, rate, cap, ingested_at)` version key
  `(effective_from, product, component)`
- `telegram_offset(account_id, offset)` and `kill_switch(account_id, set_at, reason)`
- `runs(run_id, mode, git_sha, config_hash, snapshot_id, ledger_head_seq, account_id, started_at)`
- `events(seq, run_id, account_id, ts, type, payload jsonb, prev_hash, hash)` append-only;
  a trigger rejects UPDATE and DELETE
- `experiments(id, hypothesis, primary_metric, holdout_start, registered_at, verdict,
  change_note)`
- Positions and P&L are read models rebuilt from `events`, never a source of truth.

`snapshot_id` = sha256 over every `bars`, `adjustments`, `symbols`, `holidays` and
`charge_schedule` row with `ingested_at <= run.started_at`, taking the latest version of
each key, iterated in sorted key order; `replay` applies the same predicate, so a later
backfill or a revised bhavcopy cannot change what an old run saw. `runs.ledger_head_seq`
records the last event the run could see, and `replay` rebuilds positions from `events`
with `seq <= ledger_head_seq`. Everything hashed uses RFC 8785 canonical JSON, map
iteration is sorted, and wall-clock timestamps are excluded from hashed payloads. Together
with `git_sha` and `config_hash` this makes any run reproducible byte-for-byte.

### Not-yet-published sessions vs. holidays in `backfill`

`ingest_log(source, date, status, rows, note)` (M0) records one row per fetch attempt;
`LoggedDates` treats a date as permanently settled once it has a row with status `ok` or
`no-file`, so `backfill` never refetches it. That makes an HTTP 404 from NSE's archive a
load-bearing judgement: a 404 usually means a non-trading day, but for a date NSE has not
published yet it only means "not available *yet*". NSE posts each session's bhavcopy in
the evening IST, and `backfill --to` defaults to today, so settling every 404 as `no-file`
would let a single early run mark a real trading day absent forever -- an insert-only
`bars` table can never be corrected by a later run, and the only recovery would be an
operator deleting the stray `ingest_log` row by hand.

`Backfill` therefore settles a 404 only once the session date is safely past: a full day
(`noFileSettleLag`) after that date ends at midnight IST, which clears the evening publish
window with a day to spare. A 404 inside that window is counted in the run's `no-file`
total and reported on stderr, but **no `ingest_log` row is written**, so the next run
fetches the date again and picks up the archive as soon as NSE publishes it. The cost is
that a genuine holiday close to today is re-requested on one or two later runs before it
settles; the benefit is that `backfill --to` at or near today is safe to run at any hour,
and no trading day can be silently lost.

### Evening flow (phase 1)

Scheduled on the VPS from phase 1 (Publisher needs no static IP, but a laptop that sleeps
cannot be trusted with a cron). If it must run on the Mac, launchd plus a `pmset` wake.

1. `ingest` (19:00 IST): pull eod2 output for T, insert bars and adjustments as new
   versions (never update), compute snapshot_id. If T's bhavcopy is absent by 20:30 the evening aborts with an
   `evening.aborted` event and a Telegram notice; nothing is proposed for T.
2. `evening`: freeze the PIT universe for T; run the engine on WallClock; strategies emit
   intents; RiskGate approves and sizes; CostModel stamps expected charges and slippage;
   the Paper broker also runs every evening and its book is the **shadow book** that
   drawdown and tracking error are measured against; every step appends a hash-chained
   event. Holdings the exchange delisted close at their last close with a `delisted` event.
3. `forward`: RFC 8785 canonical JSON of the proposed order set → sha256 → commit to the
   public forward-test repo before 09:15 next day; reveal after the next close.
4. `notify` (inside the same `tick`): one Telegram message with the orders, the reasons,
   expected cost, and a "Confirm in Kite" link. The link opens a static page hosted on the forward-test repo's
   GitHub Pages; the basket JSON travels in the URL fragment (never sent to a server) and
   the page auto-submits the Publisher basket form with the Publisher api_key, which is a
   client-side key by design. Orders become AMOs after hours. Kite baskets cap near 20
   orders, so a rebalance emits as many links as it needs. Inline buttons record accept or
   skip with a reason; the next `tick` drains them, so no inbound port.
5. Reconciliation runs in the `tick` that receives the day's token. The Kite login's
   redirect URL is the same GitHub Pages page, which displays the `request_token`; the human
   sends `/token <value>` to the bot; the binary exchanges it using the api_secret from the
   env file, holds the access token in memory only, and reconciles fills and actual charges
   from Kite orders and holdings (free personal plan) into `order.filled` events.
   Reconciliation matches on the Kite order `tag` = run_id where the basket API accepts a
   tag, else on symbol, side, quantity and date. Holdings without a matching ledger order
   are outside the algo book (the core SIP and manual trades share the account) and are
   ignored. An accepted order with no fill (AMO rejected, circuit-locked open) re-emits the
   next evening if the intent still holds; it is never chased.
6. `replay --date T` regenerates step 2 from snapshot_id + git_sha and diffs byte-for-byte.
7. `verify` evaluates the promotion gate and kill criteria from the ledger, weekly.

### Backtest flow

Same engine on SimClock over the date range. Fills at T+1 open with modelled slippage;
no fill on a day the open is circuit-locked. PIT universe per date. Costs from the dated
schedule. Sizing goes through `internal/risk` exactly as live. Output: equity curve, net vs gross,
versus Nifty 50 TRI, trade list, turnover, cost drag, per-trade mean and standard
deviation (used to set gate thresholds honestly). `holdout_start` lives in config and is
committed before the first run. Every run registers an experiment row first. The refusal
rule is mechanical: a run is refused when an experiment with the same `hypothesis` has
`verdict = rejected` and either the new run's `config_hash` matches any prior run of that
experiment or `change_note` is empty.

### Phase 2 (autonomy)

The same VPS, now with its static IP whitelisted at Zerodha (an Oracle free-tier VM
works). Kite Broker adapter places AMOs through the execution API in the evening, after
the human's daily Kite login; the token is valid until the next morning's flush, which
covers evening placement. Identical RiskGate, ledger, and commit-reveal. The same kill-switch
row is checked before every order. Autonomy starts at 25% of the algo book and steps up
only while `verify` stays green. A one-page runbook.

### Scale story (what a reviewer will ask)

- Many strategies: registered rows with config, run in one engine pass.
- Many symbols: bounded by Postgres, not by the engine.
- Many accounts: account_id on every account-scoped row now; per-account Broker and
  Notifier instances are a later step, the column is what makes it a step and not a rewrite.
- Intraday later: Clock and Bar are the only abstractions that change.
- Fan-out later: events are already an append-only stream; a queue can tail it. Not now.
- Stateless binary: horizontal scale is trivial because Postgres holds all state.

### Reuse

- eod2 as the data producer (separate process, GPL-3.0 untouched).
- gokiteconnect (official, MIT, pushed 2026-08-24).
- TradeLabs (MIT): the dated statutory charge table and the point-in-time universe
  approach, ported to Go; its ledger-gated research discipline, copied as process.

## Open Questions

- Nifty 50 TRI series source for the benchmark (NSE index archive, else NIFTYBEES adjusted
  as a proxy with the difference stated).
- Nifty 500 membership history as an upgrade over the turnover-ranked universe: confirm
  how far back NSE's constituent archives go and in what format.
- Whether Kite Publisher basket items accept a `tag`; if not, reconciliation falls back to
  symbol, side, quantity, date.
- Publisher basket requires an app api_key: confirm the Publisher/personal plan is free
  for this use.
- Telegram accept/skip: polling from the binary (no inbound port) versus a webhook.
  Default polling.
- Gate thresholds (bps band, N evenings, drawdown cap) are set only after the first
  backtest reports per-trade mean and standard deviation.
- Forward-test repo: separate public repo (recommended) or a folder in the code repo.
- Exact DP charge per sell per scrip and any charge-schedule changes since the last
  TradeLabs snapshot: verified by the contract-note reconciliation in Next Steps.

### Resolved

- **Whether eod2 retains delisted symbols' history (M0).** It does not: eod2 is
  survivor-only -- DHFL and JETAIRWAYS 404 in its `daily/` directory, while NSE's own
  bhavcopy for 2015-06-30 carries both. So the raw NSE archive fetcher and a resumable
  `backfill` were built in M0 rather than deferred to M1, `--source` defaults to
  `nse-bhavcopy`, `UniverseAsOf` accepts only the two known sources, and a test
  (`TestUniverseAsOf_Eod2SourceIsSurvivorOnly`) pins the difference. This is what
  premise 7 now records: bhavcopy is the source of record, eod2 is the cross-check.

## Success Criteria

- Any backtest or evening run is reproducible byte-for-byte from `snapshot_id + git_sha +
  config_hash`.
- First strategy has a registered hypothesis, a committed holdout, and a report with net
  vs gross vs benchmark, published whichever way it goes.
- Alert phase runs unattended (daily Kite login and accept/skip excepted) across ≥ 120
  intents on ≥ 8 intent-bearing evenings with ≥ 99% replay fidelity.
- Public forward-test repo holds 60+ daily hashes with matching reveals.
- Golden tests: one real contract note reproduced to the paisa; one known split day
  reconciles; the CI backtest on the frozen fixture matches its committed result.
- A README a hiring engineer can read in ten minutes that explains the seams, the ledger,
  and the gate.

## Distribution Plan

- Single Go binary via GitHub Releases (goreleaser): linux/amd64, linux/arm64 (Oracle's
  always-free VMs are Ampere arm64), darwin; all from M1.
- GitHub Actions from M1: lint, `go test ./...`, golden backtest on the frozen fixture,
  release on tag.
- Phase 1 already runs on the VPS (binary plus systemd timer by scp; Postgres via Docker
  Compose). Phase 2 adds only the static-IP whitelist and the Kite adapter.
- The basket page is static HTML on the forward-test repo's GitHub Pages.
- eod2 runs from its own cron entry with its own Python venv.

## Next Steps

- **M0, scaffold (week 0):** new public repo, go.mod, Postgres compose, goose (sqlc deferred to M2, when typed read models over the ledger arrive), Go toolchain as go.mod declares (pgx and goose enter go.mod when first imported), eod2
  producer cron, `ingest` for 15 years across the turnover-ranked universe. Tests: a known
  split day reconciles, and the 2015 universe contains a since-delisted name. No CI yet.
- **M1, verdict (weeks 1 to 2):** `internal/cost` with the golden contract-note test;
  `internal/risk` (max_positions, min notional, stock and book caps) wired into backtest
  sizing; engine + SimClock + Paper broker; both strategies registered, the 200-DMA ETF
  control paper-only; CI green with the frozen-fixture golden backtest; register the first
  hypothesis, commit `holdout_start`, run the first backtest, publish the report with
  per-trade mean and standard deviation.
- **M2, ledger (week 3):** hash-chained events, provenance stamps, `replay`, experiments
  table and the mechanical refusal rule, `kill_switch` table, daily loss cap on the events
  read model.
- **M3, evening (week 4):** WallClock, `tick` on a systemd timer on the VPS, static basket
  page on GitHub Pages, Telegram notify with accept/skip drained by `tick`, `/token`
  handoff and fill reconciliation by tag, forward-test repo, `internal/gate` with `verify`.
  Start the alert phase.
- **M4, alert phase (6 to 12 months):** weekly `verify`, weekly build-in-public post.
- **M5, autonomy:** static IP whitelist, Kite adapter, `--execute`, runbook, 25% sizing.
- Later: Next.js dashboard reading the ledger; intraday via a new Clock and Bar.
- **Cost golden test, before `internal/cost`:** take one recent equity delivery contract
  note and reproduce every charge line to the paisa from the statutory rates (brokerage,
  STT, exchange transaction, SEBI, stamp duty, GST, DP charge), recording every rate that
  had to be looked up and every line that did not reconcile. That table becomes the golden
  test for `internal/cost`; charges are the number that decided the sign in 38 of
  TradeLabs' experiments.

## Reviewer Concerns

Three rounds of adversarial review (scores 7, 7, 8 of 10). The four issues from the final
round (reference-table versioning keys, the `tick` runtime replacing a oneshot timer,
risk/gate/control-strategy milestone placement, a mechanical refusal rule) were applied to
this document after that round and have not been re-reviewed.

## Implementation Notes

- **Task 2 (db package):** `go mod tidy` against goose/v3's latest release (v3.28.0)
  bumps go.mod's `go` line to `1.26.0` because that release's own go.mod requires
  go >= 1.26.0, and the locally installed toolchain is go1.25.6. To honour "keep the
  go 1.25 line," goose was pinned to v3.27.0 (its go.mod requires exactly go 1.25.0)
  via `go get github.com/pressly/goose/v3@v3.27.0` before running `go mod tidy` — an
  explicit, checksum-verified version selection via the `go` tool, not a hand-edit of
  the require block. One unavoidable side effect: `go mod tidy` still rewrites the `go`
  line from the shorthand `go 1.25` to the canonical `go 1.25.0`, because both pgx v5.10.0
  and goose v3.27.0 declare `go 1.25.0` in their own go.mod and Go's tooling normalizes
  to that literal string even though `1.25` and `1.25.0` denote the same minimum
  version. No toolchain upgrade occurs; `go1.25.6` builds and runs everything. If a
  future task needs a newer goose, revisit whether the go1.26 toolchain bump is
  acceptable then.
- **Task 2 (db package):** `db.Migrate` adds one line not in the brief's listing —
  `goose.SetLogger(goose.NopLogger())` before `goose.SetBaseFS`. Without it, goose
  writes plain `log` lines ("OK 0001_market.sql ...", "goose: successfully migrated
  database to version: 1") straight to stdout on every call, which fails the "test
  output must be pristine, no stray logs" bar (`go test -v` output would otherwise carry
  them). The line changes no signature, no SQL, no schema, and no other behaviour; it
  only silences goose's own informational logging. `newMigrateCmd` still prints its own
  "migrations applied" line on success, so the CLI keeps useful output.
- **Task 4 (bar store, `ingest eod2`):** the brief's Step 6 verification expects
  `eod2: parsed 13 bars, inserted 13 new versions, ...` against
  `internal/market/eod2/testdata`. Running the exact command specified reports
  `parsed 15 bars, inserted 15 new versions` instead (`0` on the second run, as
  expected). This is not an implementation deviation: cleanup commit `d96470f`
  (landed after the brief was written, and which the task instructions say not to
  revert) added `internal/market/eod2/testdata/daily/reliance.CSV` — a 2-data-row
  fixture with an upper-case `.CSV` extension — specifically to exercise that same
  commit's case-insensitive extension match in `eod2.LoadDir`. 13 (tatasteel.csv) + 2
  (reliance.CSV) = 15. No code in `store.go` or `main.go` differs from the brief; only
  the fixture directory's contents changed underneath it between brief-authoring and
  implementation. Idempotency (`inserted 0` on the second run) holds exactly as
  specified.
- **Task 6 (point-in-time universe, `universe.go`/`universe_test.go`):** `universe.go`
  and the `universe` command are exactly as the brief specifies, no changes, **except
  one fix-round-1 addition**: `UniverseAsOf` now rejects an unrecognized `source` before
  querying (`if source != SourceBhavcopy && source != SourceEod2 { return nil,
  fmt.Errorf(...) }`, inserted right after the existing `lookbackDays`/`n` validation, no
  SQL or signature change). Without it, `verdict universe --source <typo>` printed only
  the header row and exited 0 — a silently "successful" but meaningless result
  indistinguishable from "no symbols qualify this window", since `source` is a free-form
  flag with no allow-list anywhere in the brief's given code. Placed in `UniverseAsOf`
  itself (not the CLI command) so every current and future caller gets the check, not
  just `newUniverseCmd`. This is a deviation from the brief's verbatim Step 3 listing,
  disclosed here per the fix-round instructions; it doesn't touch the brief's SQL or
  `UniverseAsOf`'s signature. One test beyond the brief's Step 1 listing was added:
  `TestUniverseAsOf_AsOfIngestIsolatesRevisions`. The brief's three given tests all pass
  `time.Now()` as `asOfIngest`, so none of them exercises `UniverseAsOf`'s own
  point-in-time parameter — the `ingested_at <= $5` predicate that is this project's
  whole reason for being an insert-only, versioned store. The added test inserts one
  bhavcopy day, records a timestamp, revises one symbol's turnover (a second version,
  same version key), then asserts `UniverseAsOf` as-of the earlier timestamp returns the
  original turnover, as-of now returns the revised turnover, and both rows remain in
  `bars` (2 versions for that symbol/date/source). Confirmed to actually discriminate by
  temporarily flipping the two expected values and watching the test fail on the correct
  values before reverting. No SQL, signature, or naming from the brief was touched.
- **Task 6 (pre-existing test-infra finding, mitigated in fix round 1):** `go test ./...` (the
  Makefile's `test` target) is intermittently flaky with a `duplicate key value violates
  unique constraint "bars_pkey"` error from `InsertBars`, reproducing on roughly half of
  attempts once `universe_test.go` exists. Root cause: `internal/db/db_test.go`,
  `internal/market` (via `testutil.Pool`), and `internal/market/bhavcopy` (via
  `testutil.Pool`) all point at the *same* physical `verdict_test` database and each
  independently runs its own `TRUNCATE bars, symbols ... RESTART IDENTITY` at test setup
  with no cross-process coordination; Go's default `go test ./...` runs different
  packages' test binaries concurrently. `EnsureSymbols` (`store.go`) assigns each new
  ISIN its `symbol_id` with its own separate, non-transactional `INSERT ... RETURNING`
  call in a per-ISIN loop rather than one batch/transaction; when a concurrently-running
  package's `TRUNCATE ... RESTART IDENTITY` lands mid-loop, the sequence resets to 1
  partway through, and two different ISINs in the same `InsertBars` call can end up
  mapped to the same `symbol_id` — a real primary-key collision on `(symbol_id, date,
  source, ingested_at)`, not a bug in `UniverseAsOf`. This race pre-dates Task 6 (it
  lives in Task 2's `db_test.go` truncate and Task 3/5's `EnsureSymbols`), but Tasks 2-5's
  own tests use 1-2 bars so the vulnerable window is too short to trigger it often.
  Task 6's brief-specified tests load a full bhavcopy day (hundreds of EQ rows), which
  keeps `EnsureSymbols`'s loop open long enough to make the race land reliably.
  Confirmed by: (a) running the exact pre-Task-6 tree repeatedly — clean every time; (b)
  running with Task 6's files added — fails on roughly half of attempts; (c) running
  `go test -p 1 ./...` (forcing sequential package execution) with Task 6's files present
  — clean every time. **Fix round 1 update:** three independent re-reviews flagged that
  disclosure alone left the repository's documented, default test command
  (`go test ./...` / `make test`) broken for anyone who clones this repo, and that this
  is not acceptable to ship unmitigated. Rather than the reviews' `-p 1`-in-the-Makefile
  suggestion (which only protects invocations that go through `make test`, and forces
  every *other*, non-database package to run sequentially too), landed the reviews'
  other explicitly-offered alternative — "serialize the DB-touching packages" — directly
  in the code every one of them already shares: `testutil.Pool` (`internal/testutil/testdb.go`)
  now acquires a fixed-key Postgres session advisory lock (`pg_advisory_lock`) on a
  dedicated connection before its `TRUNCATE`, and holds it for that `Pool`'s whole
  lifetime (released, via `t.Cleanup`, only after the test's pool closes). Every
  `Pool(t)`-backed test across every package now serializes against every other one,
  regardless of how `go test` is invoked, closing the gap a Makefile-only fix would have
  left (a bare `go test ./...`, an IDE test run, or a differently-invoked CI step would
  all still race). `internal/db/db_test.go` — the third culprit named above, which never
  used `testutil.Pool` and ran its own ad hoc `TRUNCATE` — was changed to call
  `testutil.Pool(t)` for its connection and truncate too (its explicit double-`db.Migrate`
  call, which exists to assert `Migrate` is idempotent, is unchanged; `testutil.Pool`
  migrating a third time inside is a harmless no-op that reinforces the same assertion),
  so all three original culprits now route through the one locked path. `EnsureSymbols`'s
  underlying non-transactional per-ISIN insert loop is untouched and still not itself
  race-free in the abstract — this closes the concurrency the race depends on within this
  repository's own test suite, it does not make `EnsureSymbols` safe against arbitrary
  concurrent external callers, which was never Task 6's job to fix. Verified with the
  reviews' own reproduction method: `VERDICT_TEST_DATABASE_URL=... go test -count=1 ./...`
  (the bare, Makefile-bypassing command that failed 2/3 runs before this fix) run 8
  consecutive times after it, clean every time (see the fix-round-1 report). Follow-up
  recommendation for the underlying non-transactional loop itself still stands: wrap
  `EnsureSymbols`'s per-ISIN inserts in one statement/transaction.


### Fix round 2 (final whole-branch review of M0)

- **`backfill` no longer skips weekends.** The walk filtered Saturdays and Sundays before
  `ingest_log` was read or written, so a weekend date was never attempted, never logged, and
  no later run could notice it was missing. NSE holds live sessions on some weekends (Muhurat
  trading, Budget Saturdays, special live-trading and DR sessions) and 16 such dates between
  2011 and 2026 carry real bars, all of them present under `eod2` and none under
  `nse-bhavcopy`. Every calendar date now goes through `Fetch`; a weekend with no session
  404s and settles as `no-file` down the same path a holiday takes, at the cost of roughly
  40% more requests, all cheap 404s.
- **`symbols` gained `valid_from`** (migration `0002`), the session date a ticker was
  observed for. Reads previously resolved the label with `ORDER BY ingested_at DESC LIMIT 1`,
  so interleaving `ingest eod2` (which maps a ticker's whole history to today's name) with a
  historical `backfill` made a symbol's ticker oscillate and labelled a 2015 bar with
  whichever name landed last. `BarsForDate` and `UniverseAsOf` now resolve the label at the
  bar's own date, keeping the as-of-ingest axis alongside the as-of-calendar one.
- **`EnsureSymbols` is now one transaction under a fixed-key advisory lock**, closing the
  follow-up recorded at the end of the fix-round-1 note above. Its read-then-insert loop
  could give one new ISIN two `symbol_id`s when `backfill` and `ingest eod2` ran together,
  with every constraint satisfied and no way to repair it in an insert-only store.
- **`UniverseAsOf` returns the realised session count.** It silently shortened its ranking
  window to whatever history was loaded, so mid-backfill a `--lookback 125` call returned a
  two-session ranking indistinguishable from a six-month one. `verdict universe` prints the
  realised window above the table.
- **Both loaders reject non-finite and out-of-range numbers.** `strconv.ParseFloat` accepts
  `nan`, `inf` and `-inf` with a nil error, pgx writes those into a `numeric` column, and
  Postgres orders NaN above every real number, so one poisoned close would have ranked first
  in the universe. `market.Finite` and `market.Count` are the shared guard.
- **`testutil.Pool` takes its advisory lock before `db.Migrate`, not after.** goose's legacy
  Up path takes no lock and its Postgres dialect issues a bare `CREATE TABLE
  goose_db_version`, so on a virgin database three concurrent test binaries raced; verified
  that three concurrent `db.Migrate` calls against a fresh database fail two of three.
- **`docker-compose.yml` binds Postgres to `127.0.0.1`.** Compose's short port syntax
  defaults the host IP to `0.0.0.0`, so the committed file published the superuser role, with
  the password written in the same public file and TLS disabled, on every interface -- and
  the Distribution Plan above names this file as the phase-1 VPS deployment mechanism.
  Remote access, if a later milestone needs it, arrives with a password from the environment
  and `sslmode=require`, recorded here as a decision.

### Known limitation: ISIN succession (open, M1)

`symbols` keys identity on ISIN, but NSE reissues an ISIN on a face-value split, so one
company becomes several disjoint `symbol_id`s: Tata Steel is `INE081A01012` before its 2022
1:10 split and `INE081A01020` after, and 534 of 4,092 tickers in the working database already
hold more than one `symbol_id`. The consequences are real. History fragments at the split
date; the 80%-presence rule in `UniverseAsOf` then drops a large cap out of the point-in-time
universe for roughly six months around each change; and the two sources disagree about
identity for the same company on the same date, which is precisely the cross-check two
independent implementations are meant to buy.

This was left open deliberately rather than patched. Preventing new fragmentation is the
easy half (an insert-only `isin_aliases(symbol_id, isin, ingested_at)` table consulted before
a `symbol_id` is minted). Repairing the existing half is not: `bars` rows carry the
`symbol_id` assigned at insert time and the table is insert-only, so a retroactive merge
needs either a full re-ingest or a canonical-entity layer resolved at read time through both
readers -- a change to what every read means, and one that has to be seeded from a heuristic
(an old ISIN whose bars stop on session N and a new ISIN under the same ticker whose bars
start on session N+1) whose false positives would merge two different companies
irreversibly. That belongs in its own change with its own review, before M1's engine reads
`bars`. Until it lands, the "no survivorship bias" claim holds for every name that never
changed its ISIN, and the M0 exit criterion (DHFL and JETAIRWAYS in a 2015 universe) is
unaffected -- neither ever split.

#### Stage 1 of the repair has landed (migration 0004, `symbol_links`)

The canonical-entity layer described above now exists as a read-time overlay: migration
0004 adds `symbol_links`, and `UniverseAsOf` and `BarsForDate` resolve identity through it.
**It ships with the table empty**, so every symbol is still its own entity and both reads
answer exactly what they answered before — verified read-only against the live 13,792,595-bar
store: the whole ranked universe at `--asof 2026-09-04 --lookback 125` (2,127 entities) and
`BarsForDate` for that session (2,633 rows) are row-for-row identical to the pre-change
queries, and the entity label agrees with the per-symbol label for 4,100 of 4,100 symbols on
each of six sampled dates. **That equivalence is weak evidence and must not be cited as
though it were strong**: with no link rows every entity is a singleton and the two queries
are identical by construction, so it can only rule out a regression for unlinked symbols. It
says nothing about the multi-member path. The full design, its staging and its test plan are
in `.superpowers/sdd/2026-09-07-m0-scaffold/isin-design.md`.

Two judgement calls made while implementing Stage 1, recorded here because they resolve
places where that document says two things:

1. **`verdict entities check` reports the three invariants only.** §4.6 describes the
   command as "I1/I2/I3 plus unlinked candidates", but the candidate SQL and the G0–G6 gates
   it runs are assigned to Stage 2 in §9, with the seeder. The candidate half is therefore
   absent rather than half-built, and `CheckEntityInvariants` says so in its doc comment.
2. **I3 (issuer agreement) does not fail the command.** §4.6 calls I3 "advisory, non-fatal"
   while the CLI table beside it says `check` "exits non-zero on any violation". The more
   specific statement wins: I1 and I2 exit non-zero, I3 prints and does not. A face-value
   split keeps the NSDL issuer code, so a mismatch is a question for a human, and making it
   fatal would train an operator to ignore the command.

Two gaps in Stage 1's write-time guard, found by review after the code landed and recorded
here rather than papered over. **Neither is a defect in the trigger**: the design specifies
that SQL and Stage 1 implemented it. In both cases what was wrong was the claim being made
about what the SQL guarantees, so the claim is what changed.

1. **The flatness trigger guarantees the map is flat *as resolved at `now()`*, not "flat at
   every timestamp".** Its two `SELECT`s read `symbol_links` with no bound relative to
   `NEW.ingested_at`, so they inspect the map today's readers see. The stronger reading
   holds only while every row's `ingested_at` is non-decreasing — then the map at a past pin
   is one the trigger validated when it was current, and induction carries it — and a single
   backdated row breaks the induction. Reproduced against the live schema: write `B -> A` at
   10:00, retract `B` at 10:01, then insert `C -> B` with `ingested_at` 10:00:30. The trigger
   accepts it (at `now()`, `B` resolves to itself), `entity_map_now` is flat, and
   `entity_map_at('10:00:45')` returns `C -> B` and `B -> A`: two hops, for any read pinned
   inside that interval. Bounding the trigger to `NEW.ingested_at` would not be a fix — it
   would let a backdated row pass a check the *present* map would fail. Nothing in Stage 1
   writes a backdated row (only the test helper can) and Stage 2's `apply` takes the default,
   so the past map is checked after the fact instead: `verdict entities check
   --as-of-ingest <timestamp>` runs I1/I2/I3 at any pin. `CheckEntityInvariants` was already
   pinned; before that flag the pin was simply unreachable from the CLI.
2. **§8.6's "re-point every member or none" is enforced in the ROOT direction only.** The
   trigger raises that exact sentence when the row being written is an entity's root and
   members still hang off it. It asks nothing about the *siblings* of the symbol being
   written, so the literal case the clause names — move ONE member of a multi-member entity
   to a different root, leave its siblings behind — is accepted: check 1 asks whether the new
   root resolves elsewhere (it does not) and check 2 asks whether the moved symbol is itself
   the entity of others (it is not). The result is the silent split §8.6 exists to prevent,
   arriving from the member side, and nothing downstream catches it either: the resulting map
   is flat, so I1 is silent, and the halves are disjoint by construction, so I2 is silent.
   `TestSymbolLinksAcceptsMovingOneMemberOutOfAMultiMemberEntity` pins the accepted case in
   the shape of §8.7b — a test that asserts nothing fires — and
   `TestSymbolLinksRejectsATwoHopMap`'s doc comment no longer claims the general rule.

The rollback caveat from the migration is worth repeating here: 0004's `Down` drops the read
surface (the view, `entity_map_at`, the flatness trigger) and deliberately **not** the table
or its rows. Once any run records a `snapshot_id` computed over `symbol_links`, dropping the
table is not a rollback, it is permanent data loss — re-running `apply` mints fresh
`ingested_at` values that are not the ones the snapshot hashed. A consequence to know before
using it: because the table survives `Down`, migration 0004 is not re-runnable after one
without dropping the table by hand.

#### Stage 2 of the repair has landed (the seeder, the gates and the roster)

`internal/market/entities` now holds the roster type, gates G0–G6, the RFC 8785 digest and
the candidate generator, and `verdict entities propose|link|apply|retract` exist beside
`check`. **The roster is generated and committed; it has NOT been applied to the live
`verdict` database.** Writing ~444 merge rows into a store that cannot delete is a decision
for a human who has read the diff, and this branch stops one step before it. `apply` was
exercised against `verdict_test` instead, which is what test 8.14 requires.

What the generator measured, read-only, against the live 13,792,595-bar store (archive
2011-09-02..2026-09-07, 3,722 sessions, 4,089 symbols with bhavcopy bars):

| | |
|---|---|
| candidates generated | 625 (573 issuer-prefix, 52 ticker-only) |
| **auto-accepted (passed G0–G6)** | **444 links over 415 entities** |
| quarantined | 181 |
| first failure G0 (fund units, INF/IN9) | 52 |
| first failure G3 (spans overlap) | 1 |
| first failure G4 (live sessions in the gap) | 123 |
| first failure G4b (5+ calendar days) | 1 |
| first failure G6 (boundary ratio outside every band) | 4 |

The four G6 failures are the four the design names
(`INE052T01013`→`INE052T01021`, `INE726L01019`→`INE726L01027`,
`INE688J01015`→`INE688J01023`, `INE528G01027`→`INE528G01035`), and the boundary-ratio
distribution reproduces §5.3's table exactly (57 / 61 / 50 / 2 / 278 / 1 across the same
bands). The count is **444 rather than the design's "about 445"** because G4b rejects
`INE659A01015`→`INE659A01023`, a pair with zero sessions in its gap but six calendar days —
which is the single disagreement §5.4 predicted between a session-counting and a
calendar-counting generator. All eight top-of-book names are covered (HDFCBANK, ICICIBANK,
SBIN, AXISBANK, BAJFINANCE, KOTAKBANK, TATASTEEL, BEL), 29 entities have three members, and
0 of 444 predecessors hold any eod2 bar, which is the measurement that keeps eod2 as evidence
and not as a gate.

Four judgement calls made while implementing Stage 2, recorded here because each departs
from something the design says or leaves open:

1. **G4a's archive-wide refusal ignores the *pending tail*, and this is a real deviation.**
   §5.3 says `propose` refuses "while any date inside the archive span is `'error'` or
   carries no `ingest_log` row", and then reports the live store as having exactly one such
   date (2026-09-06, a Sunday) while also saying `propose` "would run clean today". Both
   cannot hold once the archive extends past that Sunday, and it now does: the span ends
   2026-09-07 and the Sunday is inside it. The refusal therefore distinguishes two cases.
   A **hole** is an unsettled date whose settlement horizon — midnight IST after the session,
   plus `bhavcopy.noFileSettleLag` — had already passed when the last fetch for the source
   ran: a run had its chance and the date is still not settled, which is exactly the state
   G4a exists to refuse. A **pending** date is one no run could have settled yet, which is
   `backfill.noFileSettled` working as designed rather than a gap in the archive. Pending
   dates are still NOT settled for any individual candidate's own G4a, so a boundary landing
   in the tail is quarantined either way — verified: one candidate
   (`INE0OPA01019`→`INE0OPA01027`, successor's first bar 2026-09-07) fails G4a for exactly
   this reason. Measured today: 0 holes, 1 pending. Without this distinction the roster
   would be ungeneratable for a reason that is a clock rather than a gap in the data.
2. **The roster carries two fields §5.1's example does not: `reason` and `digest`.**
   `reason` is `succession` or `manual` and maps 1:1 to `symbol_links.reason`, which
   otherwise had no way of ever being written. `digest` is the file's own statement of the
   sha256 §5.1 defines, so `Load` can refuse a file edited after the digest was taken —
   which is what §8.12's "`apply` refusing a roster whose digest does not match the file on
   disk" requires something to compare against. An omitted `reason` reads as `succession`
   and is normalised before hashing, so adding the word does not change the digest.
   `overlap_dates` is measured per candidate and recorded in the review file rather than in
   the roster's `gates` block, which stays exactly as §5.1 writes it.
3. **A `manual` line is exempt from G1–G4b and G6 — NOT from G0's check digit — and must
   name a ratifier and a note.** The design requires both that `Load` validate G0–G6 and
   that hand-written lines exist for the 51 fund-unit pairs — where G1 is meaningless by
   §10's own account — so the two cannot both apply to the same line. What a manual line is
   excused is G0's `^INE` half and the gates that depend on the pair being an equity
   succession at all. **The ISO 6166 mod-10 check digit is not excused, and an earlier
   revision of this note wrongly said it was.** §5.3 justifies that gate by pointing AT the
   hand-written line — it "catches a typo in a hand-written roster line, which is exactly
   where a false positive would originate" — and it is free: across all 1,250 ISINs in the
   625 candidates, 104 of them non-INE `INF`/`IN9` units and including NIFTYBEES' own
   `INF732E01011` and `INF204KB14I2`, zero fail it. The residual defence, `apply` refusing
   an ISIN this store never registered, fires only when the typo lands on nothing; a typo
   that lands on another real company's ISIN is the false positive §5.6 names, and it goes
   into a table with no DELETE. Manual lines are also still held to G3 (disjoint spans), to
   G5 (the graph must be a path) and to the boundary being the successor's first bar,
   because those are what the store itself will enforce or silently mis-answer.
4. **`apply` warns about unratified lines rather than refusing them.** The generated roster
   leaves `ratified_by` empty: `propose` cannot know who will ratify, and filling in a name
   would be the machine asserting a human's approval. `apply` prints how many lines name no
   ratifier and writes them anyway, because the provenance that matters — the sha256 of the
   exact file, in every row — is recorded either way, and refusing would make a scratch
   store unusable. A reviewer merging the roster PR is expected to fill the field in.

#### Stage 2, fix round 1: five more judgement calls, and one measurement

Recorded here for the same reason as the four above — each departs from something the
design says, leaves open, or claims.

5. **G6 admits an arbitrary cross-company splice about two times in five. §5.3 says
   "probability close to 1" of rejecting it; that is the one number in the design this
   implementation cannot reproduce.** The band rule is the design's and is unchanged — the
   design is the authority — but the residual risk is now sized instead of asserted.
   Measured read-only against the live store: at the four real boundary dates 2014-07-30,
   2016-09-09, 2019-09-20 and 2022-07-29, every successor-side close over every OTHER
   company's close on the prior session gives **9,311,610 genuine "a different company's
   price spliced in here" ratios, of which 3,859,740 — 41.5% — land inside a G6 band.** The
   cause is the union of the nine bands at ±25%: `[0.0075,0.0125] ∪ [0.015,0.025] ∪
   [0.0375,0.0625] ∪ [0.075,0.125] ∪ [0.15,0.625] ∪ [0.75,1.25]`, where the 2/5 band fuses
   1/4, 1/5 and 1/2 into one 0.475-wide interval covering the middle of the plausible
   range. In log space over [0.0075, 2] the accepting set covers 71%. Since §5.6 establishes
   that post-hoc detection from inside the store is nil for a link that passed G1 and G4,
   G6 is the only non-structural thing standing behind 444 irreversible rows, and the human
   running `apply` should size it as *weak evidence* rather than as near-certain rejection.
   The cost of tightening, if it is ever wanted, is now known: **±20% loses exactly 1 of the
   444 accepted lines** (the one at ratio 0.605), breaks the fused interval into
   `[0.16,0.30] ∪ [0.32,0.60]`, and drops the false-accept rate to 34.8% (59% in log
   space); **±15% loses 23 lines**, which is well past §5.4's budget.
6. **`boundary_delivery_ratio` is `null` on all 625 records in
   `data/succession-review.jsonl`, and structurally always will be.** §5.6's second
   consequence names the review file's content as "the close ratio, the turnover ratio and
   the delivery ratio across the boundary", so a third of the material is inert. The cause
   is not a data gap that might close: `SELECT count(*) FILTER (WHERE delivery_qty IS NOT
   NULL) FROM bars WHERE source='nse-bhavcopy'` is **0 of 6,031,245** because the bhavcopy
   parser never reads `DELIV_QTY` (`internal/market/bhavcopy/parse_test.go` pins
   `require.Nil(t, dhfl.DeliveryQty)`). The field is kept rather than dropped so the record
   shape does not change under a reviewer mid-review, but **the review file carries close
   and turnover only**, and a reviewer must not read a null there as "delivery was flat".
7. **§4.1's growth path for an OLDER predecessor is not implementable against §4.2's
   CHECK, and `apply` now refuses that roster instead of failing inside a trigger.** §4.1
   says a predecessor older than the current root "joins by pointing at the existing
   `entity_id` rather than renaming the entity". `plan` derives every row's entity from the
   roster's own chain root, so a roster that gains such a predecessor renames the entity —
   and migration 0004's flatness trigger then rejects the insert with `symbol_links: N is
   the entity of other symbols; re-point every member or none`, which is advice §4.1 and
   §10 both forbid taking. The row §4.1 wants is `(symbol P, entity A)` with **no
   predecessor to record**, and §4.2's CHECK requires `predecessor IS NOT NULL` on every
   non-retraction row, so the two sections are not compatible and this is reported back to
   the design rather than improvised around. What changed in the code is only the failure:
   `apply` runs both flatness guards before the INSERT and refuses with an error that names
   the entity, its members and the contradiction. **The backward-extension path stays
   unavailable until the design settles the row shape**; forward extension (a fresh split)
   is unaffected and works.
8. **`apply` refuses a roster whose `boundary` or `predecessor` disagrees with the row
   already in the store, instead of reporting "already linked".** Deciding "already linked"
   on `entity_id` alone made a re-apply of a CORRECTED roster a silent no-op — and since §0
   withdrew revision 1's claim and made `boundary` the only column that says which member
   is in force on a date, that is a silent failure to correct a load-bearing value. It is
   reachable from the monthly ops flow: `bars` is insert-only, but a backfill can still add
   EARLIER sessions (§5.3 cites 19 previously-unknown weekend sessions added by one run),
   which moves a successor's first bar and hence `effective_from`. The comparison is on
   `boundary` and `predecessor` — what the row MEANS — and deliberately **not** on
   `roster_sha`: the sha covers the whole file, so treating a changed one as a change to
   every row would rewrite all 444 rows for an edit to one line, into a table with no
   DELETE. A stale `roster_sha` on an unchanged row is still accurate — that row was
   authorised by that roster. Correcting a boundary is `retract --symbol` then `apply`,
   which the error names; the new row supersedes the old one and both stay replayable.
9. **`retract --symbol` refuses whenever the entity has more than one linked member, not
   only when the named symbol is a root.** §4.6 retains `--symbol` "as an alias only for the
   single-member case"; implementing only the literal half of that sentence let a NON-root
   member of a three-member chain through, detaching it and leaving its siblings pointed at
   the old root — one company split across two entity ids, with a hole in the middle of the
   survivor's history, reported as "entity N dissolved". Nothing downstream sees it: the
   resulting map is flat (I1 silent), disjoint (I2 silent) and same-issuer (I3 silent),
   which is the blind spot already recorded above for the trigger, reached from a supported
   CLI flag. The guard now implements the whole sentence, and it collapses to the previous
   behaviour for a two-member entity. The CLI also no longer prints "entity N dissolved"
   for a symbol-scoped run; it prints what happened.
10. **`propose` quarantines a pair that a human has retracted, and `apply` warns when it
    re-links one.** `propose` reads `bars`, `symbols` and `ingest_log` and nothing else, so
    a pair retracted for merging two different companies passes G0–G6 again next month and
    is regenerated into the roster — which `propose` overwrites wholesale — and `apply`
    re-links it without a word, because a retraction row sets `entity_id = symbol_id` and
    the "already linked" skip cannot see it. §5.6's "a wrong merge is undone by writing one
    more row" is the single argument that beat the ratified rebuild, and §9 makes `propose`
    a monthly item, so the undo would survive only if a human remembered to hand-delete a
    line from a machine-generated file every month, forever. The quarantine is reported as
    `Q retracted pair` — it is **not** a gate, and G0–G6 are unchanged — and it carries the
    retraction's timestamp and note into the review file. It quarantines rather than
    refuses because §8.4 requires re-linking after a retraction to stay possible; `apply`
    still writes the link back and now says, per row, that it is undoing an undo.
    Consequence worth stating: `propose` reads `symbol_links` when the table exists and
    **degrades to no retraction memory when it does not**, which is the live store's state
    today (§0). It says which of the two it did on every run, because a report that stayed
    silent would be claiming a check it never ran.
