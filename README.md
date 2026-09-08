# Verdict Machine

A daily-bar NSE strategy lab that earns autonomy. Personal research software:
it backtests a strategy honestly, proposes orders each evening for a human to
confirm, and grants itself limited autonomy only after proving it behaves
exactly as the backtest said. Nothing here is financial advice.

The design, including every decision and its reason, is in
[docs/DESIGN.md](docs/DESIGN.md).

## Status

M0 scaffold: Postgres schema, two data loaders (eod2 adjusted daily CSVs and
raw NSE bhavcopy archives), an insert-only versioned bar store, a
point-in-time turnover-ranked universe, and the canonical-entity overlay that
keeps a company whole across an ISIN change. The overlay ships with its map
empty: the reviewed roster is committed but applying it to a store that cannot
delete is a human's decision.

## Quick start

```bash
make db-up              # Postgres 18 on localhost:5433 (docker)
make migrate            # apply schema
make test               # unit + integration tests
make build              # bin/verdict
```

## Data

Every `bin/verdict` command reads its connection string from `--database-url`
or `$VERDICT_DATABASE_URL`, and the eod2 commands below use `$EOD2_DIR`.
Nothing in this repository loads `.env` for you, so export them first:

```bash
export VERDICT_DATABASE_URL=postgres://verdict:verdict@localhost:5433/verdict?sslmode=disable
export EOD2_DIR=$HOME/.local/share/eod2
```

- `scripts/eod2-sync.sh` clones and updates [eod2](https://github.com/BennyThadikaran/eod2)
  (GPL-3.0, runs as a separate process; this repo only reads its CSVs).
- `bin/verdict ingest eod2 --dir $EOD2_DIR/src/eod2_data` loads the adjusted series.
- `bin/verdict backfill --from 2011-09-01` pulls raw NSE bhavcopy archives, which
  still contain delisted names, so the universe has no survivorship bias.
- `bin/verdict universe --as-of 2015-06-30` prints the top 500 by median turnover. The
  `last_break` column is the date a name's ISIN changed (a face-value split): its prices
  are unadjusted on both sides of it, so a return computed across one is wrong by the split
  factor.
- `bin/verdict entities check` runs the canonical-entity invariants and reports successions
  the map has not been told about. It is a monthly item, and it exits non-zero on an I1/I2
  violation, on an *accepted* succession the map does not carry, and on a hole in the archive
  the candidates were read from; quarantined pairs and advisory I3 findings print without
  failing. It also re-runs the gates against the links the map already holds and prints a
  `STALE` line for every merged pair they no longer accept -- reported, not fatal, because an
  applied hand-written line fails a gate by construction. Against the live store it exits non-zero today with 444 missing links, which is the
  control reporting a roster that is committed and not yet applied. NSE reissues an ISIN on a
  split, so one company owns several `symbol_id`s; `docs/DESIGN.md` has the whole story under
  "ISIN succession".

The `mkdir -p` below is not decoration: the shell sets up the `>>` redirection
before it runs anything, so on a machine where eod2 has never been cloned the
whole entry fails and the log that would have said so is the file that could
not be created.

```
# crontab -e  (19:30 IST on weekdays; eod2 needs NSE's reports published after 19:00)
30 19 * * 1-5 mkdir -p $HOME/.local/share/eod2 && EOD2_DIR=$HOME/.local/share/eod2 /path/to/verdict-machine/scripts/eod2-sync.sh >> $HOME/.local/share/eod2/sync.log 2>&1
```

## Layout

```
cmd/verdict/                CLI
internal/db/             connection + embedded goose migrations
internal/market/         Bar, Store (insert-only, versioned), universe
internal/market/eod2/    eod2 CSV loader
internal/market/bhavcopy/ NSE bhavcopy archive loader + backfill
internal/market/entities/ succession candidates, the gates, the reviewed roster
scripts/                 eod2 producer
docs/                    DESIGN.md and plans
```
