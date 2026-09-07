package market

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Source names. A bar's source decides how its prices are read: eod2 is
// adjusted in place, nse-bhavcopy is unadjusted and carries rupee turnover.
const (
	SourceEod2     = "eod2"
	SourceBhavcopy = "nse-bhavcopy"
)

// Store is the insert-only, versioned bar repository.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool exposes the underlying pool for tests and ad-hoc queries.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// symbolRegistryLockKey is the fixed Postgres advisory-lock key EnsureSymbols
// holds for the length of its transaction. Registering an ISIN is a
// read-then-insert: snapshot the versions that exist, decide from that
// snapshot whether to mint a symbol_id, then insert. Two callers doing that
// concurrently -- `verdict backfill` and `verdict ingest eod2`, the two
// commands the README puts side by side, run as separate processes over a
// backfill that takes hours -- both read "this ISIN is unknown" and both
// mint, and every constraint on the table passes: PRIMARY KEY
// (symbol_id, ingested_at) and UNIQUE (isin, ingested_at) are both satisfied
// because the two autocommit transactions get different now() values. One
// ISIN then owns two symbol_ids permanently. Every market table is
// insert-only, so there is no UPDATE or DELETE available to repair it: the
// company is two companies forever, its bars split across both ids,
// UniverseAsOf returns the name twice and mis-ranks the window, and
// BarsForDate returns two bars for one company on one date.
//
// A transaction alone does not close this -- read committed lets both
// snapshots predate both inserts -- so the snapshot and the inserts are made
// atomic with respect to any other registrar by taking this lock first.
const symbolRegistryLockKey = 84031178

// symbolVersion is one row of a symbol's ticker history.
type symbolVersion struct {
	id        int64
	ticker    string
	validFrom time.Time
}

// versionAt returns the version in force on date d: the latest one whose
// valid_from is at or before d, or, when d predates every recorded version,
// the earliest one. versions must be sorted ascending by
// (valid_from, ingested_at).
func versionAt(versions []symbolVersion, d time.Time) symbolVersion {
	chosen := versions[0]
	for _, v := range versions {
		if v.validFrom.After(d) {
			break
		}
		chosen = v
	}
	return chosen
}

// EnsureSymbols registers every distinct ISIN in bars and returns ISIN -> symbol_id.
// A known ISIN whose ticker changed gets a new version row with the same symbol_id.
//
// The ticker a batch reports is recorded against the latest bar date in that
// batch for the ISIN -- the session the file describes -- not against the
// moment the row was written. That is what makes "what was this symbol called
// on this date" answerable, and it is what stops the ticker oscillating when
// an eod2 ingest (which maps every year of a ticker's history to today's
// ticker) and a historical backfill interleave: a new version is written only
// when the incoming ticker differs from the one in force *on that session
// date*, so replaying an old session never re-asserts an old name over a
// newer one.
func (s *Store) EnsureSymbols(ctx context.Context, bars []Bar) (map[string]int64, error) {
	type observation struct {
		ticker    string
		validFrom time.Time
	}
	want := map[string]observation{}
	for _, b := range bars {
		if b.ISIN == "" {
			return nil, fmt.Errorf("bar %s %s has no ISIN", b.Ticker, b.Date.Format("2006-01-02"))
		}
		if prev, ok := want[b.ISIN]; !ok || b.Date.After(prev.validFrom) {
			want[b.ISIN] = observation{ticker: b.Ticker, validFrom: b.Date}
		}
	}
	if len(want) == 0 {
		return map[string]int64{}, nil
	}
	isins := make([]string, 0, len(want))
	for isin := range want {
		isins = append(isins, isin)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(symbolRegistryLockKey)); err != nil {
		return nil, fmt.Errorf("symbols lock: %w", err)
	}

	history := map[string][]symbolVersion{}
	rows, err := tx.Query(ctx,
		`SELECT isin, symbol_id, ticker, valid_from FROM symbols
		 WHERE isin = ANY($1) ORDER BY isin, valid_from, ingested_at`, isins)
	if err != nil {
		return nil, fmt.Errorf("symbols: %w", err)
	}
	for rows.Next() {
		var isin string
		var v symbolVersion
		var from time.Time
		if err := rows.Scan(&isin, &v.id, &v.ticker, &from); err != nil {
			rows.Close()
			return nil, err
		}
		v.validFrom = Day(from.Year(), from.Month(), from.Day())
		history[isin] = append(history[isin], v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := map[string]int64{}
	for isin, obs := range want {
		versions := history[isin]
		if len(versions) == 0 {
			var id int64
			if err := tx.QueryRow(ctx,
				`INSERT INTO symbols (isin, ticker, valid_from) VALUES ($1, $2, $3) RETURNING symbol_id`,
				isin, obs.ticker, obs.validFrom).Scan(&id); err != nil {
				return nil, fmt.Errorf("symbols insert %s: %w", isin, err)
			}
			ids[isin] = id
			continue
		}
		// Every version of one ISIN carries the same symbol_id, so any row answers.
		ids[isin] = versions[0].id
		if versionAt(versions, obs.validFrom).ticker == obs.ticker {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO symbols (symbol_id, isin, ticker, valid_from) VALUES ($1, $2, $3, $4)`,
			versions[0].id, isin, obs.ticker, obs.validFrom); err != nil {
			return nil, fmt.Errorf("symbols rename %s: %w", isin, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

// InsertBars appends a new version of every bar whose content differs from the
// latest stored version for (symbol_id, date, source). Unchanged bars are skipped,
// so re-running an ingest is free. Returns the number of versions inserted.
func (s *Store) InsertBars(ctx context.Context, source string, bars []Bar) (int, error) {
	if len(bars) == 0 {
		return 0, nil
	}
	ids, err := s.EnsureSymbols(ctx, bars)
	if err != nil {
		return 0, err
	}
	bars, err = dedupeBars(bars)
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE bars_stage (LIKE bars INCLUDING DEFAULTS) ON COMMIT DROP`); err != nil {
		return 0, fmt.Errorf("stage: %w", err)
	}
	cols := []string{"symbol_id", "date", "source", "series", "open", "high", "low", "close", "volume", "turnover", "delivery_qty", "content_hash"}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"bars_stage"}, cols, pgx.CopyFromSlice(len(bars), func(i int) ([]any, error) {
		b := bars[i]
		return []any{ids[b.ISIN], b.Date, source, b.Series, b.Open, b.High, b.Low, b.Close, b.Volume,
			b.Turnover, b.DeliveryQty, b.ContentHash()}, nil
	}))
	if err != nil {
		return 0, fmt.Errorf("copy: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO bars (symbol_id, date, source, series, open, high, low, close, volume, turnover, delivery_qty, content_hash)
		SELECT s.symbol_id, s.date, s.source, s.series, s.open, s.high, s.low, s.close, s.volume, s.turnover, s.delivery_qty, s.content_hash
		FROM bars_stage s
		LEFT JOIN LATERAL (
			SELECT content_hash FROM bars b
			WHERE b.symbol_id = s.symbol_id AND b.date = s.date AND b.source = s.source
			ORDER BY b.ingested_at DESC LIMIT 1
		) latest ON true
		WHERE latest.content_hash IS DISTINCT FROM s.content_hash`)
	if err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// dedupeBars collapses bars within one InsertBars call that share the version
// key (symbol_id, date, source) into a single row. source is one value for
// the whole call, so the key here is (isin, date).
//
// Postgres's now() is fixed for the whole transaction, so two staged rows
// with the same version key would otherwise be assigned the identical
// ingested_at and collide on the bars primary key, aborting the entire
// transaction on an opaque unique-violation and discarding every other,
// unrelated bar in that same call.
//
// A duplicate whose content is identical to the one already kept (a repeated
// line in a source file) is dropped silently. A duplicate whose content
// differs is a genuine conflict this store's version key cannot represent
// within a single call — most commonly two rows for the same symbol and date
// under different series, which (symbol_id, date, source) does not
// distinguish — and is rejected up front with a precise error naming the
// offending symbol and date, instead of surfacing as a bare PK violation that
// rolls back the whole batch.
func dedupeBars(bars []Bar) ([]Bar, error) {
	type key struct {
		isin string
		date time.Time
	}
	kept := map[key]int{} // key -> index into out
	out := make([]Bar, 0, len(bars))
	for _, b := range bars {
		k := key{isin: b.ISIN, date: b.Date}
		if i, ok := kept[k]; ok {
			if !bytes.Equal(out[i].ContentHash(), b.ContentHash()) {
				return nil, fmt.Errorf(
					"insert: %s (%s) on %s appears more than once in this batch with different content; "+
						"the version key (symbol_id, date, source) cannot distinguish them",
					b.Ticker, b.ISIN, b.Date.Format("2006-01-02"))
			}
			continue
		}
		kept[k] = len(out)
		out = append(out, b)
	}
	return out, nil
}

// LogIngest records one fetch attempt. status is 'ok', 'no-file' or 'error'.
func (s *Store) LogIngest(ctx context.Context, source string, date time.Time, status string, rows int, note string) error {
	var n *string
	if note != "" {
		n = &note
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ingest_log (source, date, status, rows, note) VALUES ($1, $2, $3, $4, $5)`,
		source, date, status, rows, n)
	return err
}

// LoggedDates returns the dates already settled for a source: fetched with
// data ('ok') or confirmed absent ('no-file'). Errors are not settled.
func (s *Store) LoggedDates(ctx context.Context, source string) (map[time.Time]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT date FROM ingest_log WHERE source = $1 AND status IN ('ok', 'no-file')`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	done := map[time.Time]bool{}
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		done[Day(d.Year(), d.Month(), d.Day())] = true
	}
	return done, rows.Err()
}

// symbolLabelLateral builds the LATERAL subquery that resolves a symbol_id to
// the (isin, ticker) it carried on one session date, as the store knew it at
// one ingest timestamp. Those are two independent axes and a point-in-time
// store needs both: ingested_at answers "what did we believe then" (the audit
// trail), valid_from answers "what was this called then" (market time).
//
// The ordering picks the latest version in force at or before dateExpr; when
// the bar predates every recorded version -- a source that only ever reports
// today's ticker, such as eod2, registers the symbol at today's date -- it
// falls back to the earliest recorded version rather than dropping the row.
// Ties on valid_from break on ingested_at DESC, which is how a same-session
// correction supersedes the version it corrects.
//
// idExpr, dateExpr and ingestExpr are SQL fragments written by this file,
// never values from outside it.
func symbolLabelLateral(idExpr, dateExpr, ingestExpr string) string {
	return `(
		SELECT isin, ticker FROM symbols
		WHERE symbol_id = ` + idExpr + ` AND ingested_at <= ` + ingestExpr + `
		ORDER BY (valid_from <= ` + dateExpr + `) DESC,
		         CASE WHEN valid_from <= ` + dateExpr + ` THEN valid_from END DESC,
		         valid_from,
		         ingested_at DESC
		LIMIT 1
	)`
}

// BarsForDate returns the latest version of every bar for one source and date,
// as it was known at asOfIngest (rows ingested later are invisible). Each bar
// carries the ticker its symbol held on that session date, not today's.
func (s *Store) BarsForDate(ctx context.Context, source string, date, asOfIngest time.Time) ([]Bar, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (b.symbol_id) sym.isin, sym.ticker, b.series, b.date,
		       b.open::float8, b.high::float8, b.low::float8, b.close::float8, b.volume,
		       b.turnover::float8, b.delivery_qty
		FROM bars b
		JOIN LATERAL `+symbolLabelLateral("b.symbol_id", "b.date", "$3")+` sym ON true
		WHERE b.source = $1 AND b.date = $2 AND b.ingested_at <= $3
		ORDER BY b.symbol_id, b.ingested_at DESC`, source, date, asOfIngest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bar
	for rows.Next() {
		var b Bar
		var d time.Time
		if err := rows.Scan(&b.ISIN, &b.Ticker, &b.Series, &d, &b.Open, &b.High, &b.Low, &b.Close, &b.Volume,
			&b.Turnover, &b.DeliveryQty); err != nil {
			return nil, err
		}
		b.Date = Day(d.Year(), d.Month(), d.Day())
		out = append(out, b)
	}
	return out, rows.Err()
}
