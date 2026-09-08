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

// versionAt returns the version in force on date d: the latest valid_from at
// or before d, and among the versions tying that valid_from the one ingested
// last, because a later observation of the same session corrects an earlier
// one. When d predates every recorded version nothing is in force and the
// answer has to be invented: it takes the earliest valid_from instead -- a bar
// older than anything the store ever observed takes the oldest label there is,
// not today's -- resolving a tie on that date the same way, to the last
// ingested.
//
// That is the rule symbolLabelLateral implements in SQL, and the two must not
// drift apart: this function decides whether an incoming batch is a rename,
// that ORDER BY decides what a bar is called when it is read back, and a bar
// the write side and the read side label differently is one the store cannot
// answer for. The pre-history fallback is where they did drift -- this side
// took the first row of a list ordered ingested_at ascending, the SQL side
// ordered ingested_at DESC -- so a correction to the earliest version was
// visible to readers and invisible to the rename check.
// See TestSymbolLabelFallbackAgreesWriterAndReader.
//
// versions must be sorted ascending by (valid_from, ingested_at).
func versionAt(versions []symbolVersion, d time.Time) symbolVersion {
	chosen := versions[0]
	if chosen.validFrom.After(d) {
		for _, v := range versions[1:] {
			if !v.validFrom.Equal(chosen.validFrom) {
				break
			}
			chosen = v
		}
		return chosen
	}
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
		// A bar with no date would be registered at valid_from 0001-01-01,
		// which is not a date -- it is the sentinel migration 0002 gave the
		// rows that predate valid_from, meaning "no observed date; applies to
		// every bar date". Letting a caller's zero time.Time land on it would
		// make a parser bug permanently indistinguishable from the migration's
		// own backfill, in an insert-only table with no UPDATE to sort them out
		// again. The date is what makes "what was this called then" answerable,
		// so it is as required here as the ISIN is.
		if b.Date.IsZero() {
			return nil, fmt.Errorf("bar %s (%s) has no date; valid_from would collide with the 0001-01-01 pre-migration sentinel", b.Ticker, b.ISIN)
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
// today's ticker, such as eod2, registers the symbol at today's date -- the
// third key falls back to the earliest recorded valid_from rather than
// dropping the row. Ties on valid_from break on ingested_at DESC in both
// cases, which is how a same-session correction supersedes the version it
// corrects. versionAt is the Go statement of this same rule and the two are
// held to agree by TestSymbolLabelFallbackAgreesWriterAndReader.
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

// StoredBar is a bar plus the store identity it was read under. Bar itself
// stays a pure value type built by parsers that know nothing about the store.
//
// SymbolID is the physical row's own symbol -- what bars.symbol_id holds and
// what a version key is built from. EntityID is the canonical company it
// belongs to, and the two differ for every member of a succession. A caller
// grouping a company's history must group on EntityID; a caller writing bars
// must use SymbolID.
type StoredBar struct {
	Bar
	SymbolID int64
	EntityID int64
}

// BarsForDate returns the latest version of every bar for one source and date,
// as it was known at asOfIngest (rows ingested later are invisible). Each bar
// carries the label its ENTITY held on that session date -- the member in
// force on it, per entityMemberCTE -- not today's, and not necessarily the
// label of the symbol the row is physically filed under.
//
// That last clause is a deliberate change of answer and is worth stating
// plainly: eod2.LoadDir files a ticker's whole CSV under today's ISIN, so an
// eod2 bar for Tata Steel on 2015-01-02 is physically the post-2022 symbol's
// row. It now comes back as INE081A01012 / TATASTEEL, because on that session
// the ISIN really was INE081A01012. It is a move toward market time, not away
// from it, and it applies only to eod2 rows and only to queries pinned at or
// after a seed.
//
// The identity restored is IDENTITY ONLY. eod2 bars are split-adjusted in
// place and nse-bhavcopy bars are not, so for every pre-boundary session the
// two sources' closes for one entity differ by the cumulative succession
// factor (TATASTEEL on 2015-01-02: eod2 41.10, bhavcopy 410.75, exactly
// 1:10). A cross-source price comparator built on this without the read-time
// adjustments layer would read a 10x disagreement as a data error.
//
// It errors rather than returning rows when two members of one entity hold a
// bar for this source and date. That is the catastrophic false positive -- a
// roster line merging two genuinely different companies, in a store with no
// DELETE -- and collapsing it silently would halve one company's truth while
// looking exactly like a normal read. Cross-SOURCE coexistence is expected
// and untouched, because this read filters by source.
func (s *Store) BarsForDate(ctx context.Context, source string, date, asOfIngest time.Time) ([]StoredBar, error) {
	rows, err := s.pool.Query(ctx, `WITH `+
		entityMapCTE("$3")+`, `+
		entityMemberCTE("$2", "$3")+`, `+
		entityLabelCTE("$2", "$3")+`,
		latest AS (
			SELECT DISTINCT ON (b.symbol_id)
			       b.symbol_id, b.series, b.date, b.open, b.high, b.low, b.close,
			       b.volume, b.turnover, b.delivery_qty
			FROM bars b
			WHERE b.source = $1 AND b.date = $2 AND b.ingested_at <= $3
			ORDER BY b.symbol_id, b.ingested_at DESC
		)
		SELECT m.entity_id, l.symbol_id, el.isin, el.ticker, l.series, l.date,
		       l.open::float8, l.high::float8, l.low::float8, l.close::float8, l.volume,
		       l.turnover::float8, l.delivery_qty,
		       count(*) OVER (PARTITION BY m.entity_id) AS members_on_date
		FROM latest l
		JOIN entity_map m    ON m.symbol_id = l.symbol_id
		JOIN entity_label el ON el.entity_id = m.entity_id
		ORDER BY el.ticker, l.symbol_id`, source, date, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("bars: %w", err)
	}
	defer rows.Close()
	var out []StoredBar
	for rows.Next() {
		var b StoredBar
		var d time.Time
		var membersOnDate int64
		if err := rows.Scan(&b.EntityID, &b.SymbolID, &b.ISIN, &b.Ticker, &b.Series, &d,
			&b.Open, &b.High, &b.Low, &b.Close, &b.Volume, &b.Turnover, &b.DeliveryQty,
			&membersOnDate); err != nil {
			return nil, err
		}
		if membersOnDate > 1 {
			return nil, fmt.Errorf(
				"bars: entity %d has %d members holding a %s bar on %s; two members of one entity cannot "+
					"trade the same session, so the link joining them merges two different companies -- "+
					"run `verdict entities check` and retract the link before trusting this read",
				b.EntityID, membersOnDate, source, date.Format("2006-01-02"))
		}
		b.Date = Day(d.Year(), d.Month(), d.Day())
		out = append(out, b)
	}
	return out, rows.Err()
}

// Sessions returns the distinct trading dates one source holds in [from, to],
// ascending, as the store knew them at asOfIngest.
//
// It is the exchange calendar as the archive records it rather than as a
// holiday table asserts it, which is the same choice backfill makes: NSE
// trades some Saturdays (budget days, Muhurat) and a calendar built from
// weekday arithmetic misses them. M0 lost 19 real sessions to exactly that
// bug before the archive was made the authority.
func (s *Store) Sessions(ctx context.Context, source string, from, to, asOfIngest time.Time) ([]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT date FROM bars
		WHERE source = $1 AND date >= $2 AND date <= $3 AND ingested_at <= $4
		ORDER BY date`, source, from, to, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, Day(d.Year(), d.Month(), d.Day()))
	}
	return out, rows.Err()
}
