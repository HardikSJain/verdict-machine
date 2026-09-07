# Verdict Machine

A daily-bar NSE strategy lab that earns autonomy. Personal research software:
it backtests a strategy honestly, proposes orders each evening for a human to
confirm, and grants itself limited autonomy only after proving it behaves
exactly as the backtest said. Nothing here is financial advice.

The design, including every decision and its reason, is in
[docs/DESIGN.md](docs/DESIGN.md).

## Status

M0 scaffold: Postgres schema, two data loaders (eod2 adjusted daily CSVs and
raw NSE bhavcopy archives), an insert-only versioned bar store, and a
point-in-time turnover-ranked universe.

## Quick start

```bash
make db-up              # Postgres 18 on localhost:5433 (docker)
make migrate            # apply schema
make test               # unit + integration tests
make build              # bin/verdict
```

## Data

- `scripts/eod2-sync.sh` clones and updates [eod2](https://github.com/BennyThadikaran/eod2)
  (GPL-3.0, runs as a separate process; this repo only reads its CSVs).
- `bin/verdict ingest eod2 --dir $EOD2_DIR/src/eod2_data` loads the adjusted series.
- `bin/verdict backfill --from 2011-09-01` pulls raw NSE bhavcopy archives, which
  still contain delisted names, so the universe has no survivorship bias.
- `bin/verdict universe --as-of 2015-06-30` prints the top 500 by median turnover.

```
# crontab -e  (19:30 IST on weekdays; eod2 needs NSE's reports published after 19:00)
30 19 * * 1-5 EOD2_DIR=$HOME/.local/share/eod2 /path/to/verdict-machine/scripts/eod2-sync.sh >> $HOME/.local/share/eod2/sync.log 2>&1
```

## Layout

```
cmd/verdict/                CLI
internal/db/             connection + embedded goose migrations
internal/market/         Bar, Store (insert-only, versioned), universe
internal/market/eod2/    eod2 CSV loader
internal/market/bhavcopy/ NSE bhavcopy archive loader + backfill
scripts/                 eod2 producer
docs/                    DESIGN.md and plans
```
