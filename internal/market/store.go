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

// EnsureSymbols registers every distinct ISIN in bars and returns ISIN -> symbol_id.
// A known ISIN whose ticker changed gets a new version row with the same symbol_id.
func (s *Store) EnsureSymbols(ctx context.Context, bars []Bar) (map[string]int64, error) {
	want := map[string]string{} // isin -> ticker
	for _, b := range bars {
		if b.ISIN == "" {
			return nil, fmt.Errorf("bar %s %s has no ISIN", b.Ticker, b.Date.Format("2006-01-02"))
		}
		want[b.ISIN] = b.Ticker
	}

	type current struct {
		id     int64
		ticker string
	}
	known := map[string]current{}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (isin) isin, symbol_id, ticker FROM symbols ORDER BY isin, ingested_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("symbols: %w", err)
	}
	for rows.Next() {
		var isin, ticker string
		var id int64
		if err := rows.Scan(&isin, &id, &ticker); err != nil {
			rows.Close()
			return nil, err
		}
		known[isin] = current{id: id, ticker: ticker}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := map[string]int64{}
	for isin, ticker := range want {
		cur, ok := known[isin]
		switch {
		case !ok:
			var id int64
			if err := s.pool.QueryRow(ctx,
				`INSERT INTO symbols (isin, ticker) VALUES ($1, $2) RETURNING symbol_id`, isin, ticker).Scan(&id); err != nil {
				return nil, fmt.Errorf("symbols insert %s: %w", isin, err)
			}
			ids[isin] = id
		case cur.ticker != ticker:
			if _, err := s.pool.Exec(ctx,
				`INSERT INTO symbols (symbol_id, isin, ticker) VALUES ($1, $2, $3)`, cur.id, isin, ticker); err != nil {
				return nil, fmt.Errorf("symbols rename %s: %w", isin, err)
			}
			ids[isin] = cur.id
		default:
			ids[isin] = cur.id
		}
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

// BarsForDate returns the latest version of every bar for one source and date,
// as it was known at asOfIngest (rows ingested later are invisible).
func (s *Store) BarsForDate(ctx context.Context, source string, date, asOfIngest time.Time) ([]Bar, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (b.symbol_id) sym.isin, sym.ticker, b.series, b.date,
		       b.open::float8, b.high::float8, b.low::float8, b.close::float8, b.volume,
		       b.turnover::float8, b.delivery_qty
		FROM bars b
		JOIN LATERAL (
			SELECT isin, ticker FROM symbols WHERE symbol_id = b.symbol_id AND ingested_at <= $3
			ORDER BY ingested_at DESC LIMIT 1
		) sym ON true
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
