package market

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SourceNSEIndex is the daily index archive.
const SourceNSEIndex = "nse-index"

// IndexLevel is one index's session as NSE published it.
//
// IndexCode is the canonical identity and IndexName is the string printed that
// day. They differ because NSE rebranded its whole index family between 2013
// and 2015, and keying on the printed name would split one index into three
// unrelated series -- see internal/market/nseindex for the alias table and the
// continuity check that admits an entry to it.
//
// Open, High and Low are pointers because 27 of the 165 indices in a recent
// file publish only a close and print "-" for the rest; a zero there would give
// them a daily range equal to their whole level.
type IndexLevel struct {
	IndexCode    string
	IndexName    string
	Date         time.Time
	Open         *float64
	High         *float64
	Low          *float64
	Close        float64
	PointsChange *float64
	PctChange    *float64
	Volume       *int64
	Turnover     *float64
	PE           *float64
	PB           *float64
	DivYield     *float64
}

// ContentHash is a stable digest of the published values, so a re-ingest that
// changes nothing is recognisable as a no-op rather than written as a new
// version of an identical row.
func (l IndexLevel) ContentHash() []byte {
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatFloat(*p, 'f', 4, 64)
	}
	n := func(p *int64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatInt(*p, 10)
	}
	s := strings.Join([]string{
		l.IndexCode, l.IndexName, l.Date.Format(time.DateOnly),
		f(l.Open), f(l.High), f(l.Low), strconv.FormatFloat(l.Close, 'f', 4, 64),
		f(l.PointsChange), f(l.PctChange), n(l.Volume), f(l.Turnover),
		f(l.PE), f(l.PB), f(l.DivYield),
	}, "|")
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// InsertIndexLevels writes a session's levels, skipping any whose content hash
// already matches the latest version held.
//
// The skip is what makes a re-run of the backfill free rather than a source of
// duplicate versions: the table is insert-only, so a loader that wrote
// unconditionally would add a new identical row on every pass and make
// "how many times was this corrected" unanswerable.
func (s *Store) InsertIndexLevels(ctx context.Context, source string, levels []IndexLevel) (int, error) {
	if len(levels) == 0 {
		return 0, nil
	}
	seen := map[string]bool{}
	for _, l := range levels {
		k := l.IndexCode + "|" + l.Date.Format(time.DateOnly)
		if seen[k] {
			return 0, fmt.Errorf("index levels: %s appears twice for %s in one batch",
				l.IndexCode, l.Date.Format(time.DateOnly))
		}
		seen[k] = true
	}

	existing := map[string][]byte{}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (index_code, date) index_code, date, content_hash
		FROM index_levels
		WHERE source = $1 AND date = $2
		ORDER BY index_code, date, ingested_at DESC`, source, levels[0].Date)
	if err != nil {
		return 0, fmt.Errorf("index levels: reading current versions: %w", err)
	}
	for rows.Next() {
		var code string
		var d time.Time
		var h []byte
		if err := rows.Scan(&code, &d, &h); err != nil {
			rows.Close()
			return 0, err
		}
		existing[code+"|"+d.Format(time.DateOnly)] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	batch := &pgx.Batch{}
	var queued int
	for _, l := range levels {
		h := l.ContentHash()
		k := l.IndexCode + "|" + l.Date.Format(time.DateOnly)
		if prev, ok := existing[k]; ok && string(prev) == string(h) {
			continue
		}
		batch.Queue(`INSERT INTO index_levels
			(index_code, date, source, index_name, open, high, low, close,
			 points_change, pct_change, volume, turnover, pe, pb, div_yield, content_hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			l.IndexCode, l.Date, source, l.IndexName, l.Open, l.High, l.Low, l.Close,
			l.PointsChange, l.PctChange, l.Volume, l.Turnover, l.PE, l.PB, l.DivYield, h)
		queued++
	}
	if queued == 0 {
		return 0, nil
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for i := 0; i < queued; i++ {
		if _, err := br.Exec(); err != nil {
			return 0, fmt.Errorf("index levels: insert: %w", err)
		}
	}
	return queued, nil
}

// IndexCloses returns one index's closes over [from, to], ascending, as the
// store knew them at asOfIngest.
//
// There is no succession fence here and none is needed: an index is not
// reissued on a corporate action, its level is continuous by construction, and
// the only discontinuity it can suffer is a rebranding, which
// nseindex.VerifyContinuity checks before a name is ever mapped to a code. That
// is the difference between this and EntityCloses, and it is worth stating
// because the two look alike.
func (s *Store) IndexCloses(ctx context.Context, code string, from, to, asOfIngest time.Time) ([]DatedClose, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("index closes: from %s must be before to %s",
			from.Format(time.DateOnly), to.Format(time.DateOnly))
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (date) date, close::float8
		FROM index_levels
		WHERE index_code = $1 AND source = $2 AND date >= $3 AND date <= $4 AND ingested_at <= $5
		ORDER BY date, ingested_at DESC`,
		code, SourceNSEIndex, from, to, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("index closes: %w", err)
	}
	defer rows.Close()
	var out []DatedClose
	for rows.Next() {
		var d time.Time
		var c float64
		if err := rows.Scan(&d, &c); err != nil {
			return nil, err
		}
		out = append(out, DatedClose{Date: Day(d.Year(), d.Month(), d.Day()), Close: c})
	}
	return out, rows.Err()
}

// IndexNameChange is one session where the published name differed from the
// previous session's, with both names and both closes.
//
// It is the evidence behind the alias table: a rebrand must leave the level
// alone, so a changeover whose close jumps is not a rebrand but a different
// index wearing an old name.
//
// Names are compared CASE- AND WHITESPACE-INSENSITIVELY. NSE re-typesets its
// own index names -- "Nifty Midcap 100" became "NIFTY Midcap 100" in 2016 --
// and reporting that as a rename would put a false rebasing in front of a human
// on every run until they learned to ignore this check, which is the worst
// thing a check can teach.
//
// Gap is the calendar distance between the two sessions. It matters because the
// level comparison is only meaningful when they are adjacent: the same
// re-typesetting above straddled a three-month hole in the archive, and the
// +10.53% across it was the market moving, not the index being rebased. Across
// a gap the two cannot be told apart, and saying so beats guessing either way.
type IndexNameChange struct {
	Date      time.Time
	PrevDate  time.Time
	FromName  string
	ToName    string
	PrevClose float64
	Close     float64
	GapDays   int
}

// NameChanges finds them.
func (s *Store) NameChanges(ctx context.Context, code string, asOfIngest time.Time) ([]IndexNameChange, error) {
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (date) date, index_name, close::float8 AS close
			FROM index_levels
			WHERE index_code = $1 AND source = $2 AND ingested_at <= $3
			ORDER BY date, ingested_at DESC
		), seq AS (
			SELECT date, index_name, close,
			       lag(index_name) OVER (ORDER BY date) AS prev_name,
			       lag(close)      OVER (ORDER BY date) AS prev_close,
			       lag(date)       OVER (ORDER BY date) AS prev_date
			FROM latest
		)
		SELECT date, prev_date, prev_name, index_name, prev_close, close
		FROM seq
		WHERE prev_name IS NOT NULL
		  AND upper(regexp_replace(btrim(prev_name),  '\s+', ' ', 'g'))
		   <> upper(regexp_replace(btrim(index_name), '\s+', ' ', 'g'))
		ORDER BY date`, code, SourceNSEIndex, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("index name changes: %w", err)
	}
	defer rows.Close()
	var out []IndexNameChange
	for rows.Next() {
		var c IndexNameChange
		if err := rows.Scan(&c.Date, &c.PrevDate, &c.FromName, &c.ToName, &c.PrevClose, &c.Close); err != nil {
			return nil, err
		}
		c.Date = Day(c.Date.Year(), c.Date.Month(), c.Date.Day())
		c.PrevDate = Day(c.PrevDate.Year(), c.PrevDate.Month(), c.PrevDate.Day())
		c.GapDays = int(c.Date.Sub(c.PrevDate).Hours() / 24)
		out = append(out, c)
	}
	return out, rows.Err()
}

// IndexGap is a stretch where an index vanished from the archive while other
// indices kept being published.
type IndexGap struct {
	After   time.Time
	Before  time.Time
	GapDays int
}

// Gaps returns every stretch longer than minDays where one index is missing
// from sessions the archive published for others.
//
// An index disappearing for months is a real and easy thing to miss: NIFTYMIDCAP100
// is absent from 2016-04-01 to 2016-07-06 with no announcement inside the data.
// A moving average that spanned such a hole would be averaging across a
// discontinuity in TIME rather than in level, and a trend rule reading it would
// act on a mean that covers a different period from the one it thinks.
func (s *Store) Gaps(ctx context.Context, code string, minDays int, asOfIngest time.Time) ([]IndexGap, error) {
	rows, err := s.pool.Query(ctx, `
		WITH d AS (
			SELECT DISTINCT date FROM index_levels
			WHERE index_code = $1 AND source = $2 AND ingested_at <= $3
		), seq AS (
			SELECT date, lag(date) OVER (ORDER BY date) AS prev FROM d
		)
		SELECT prev, date, (date - prev) AS gap
		FROM seq WHERE prev IS NOT NULL AND (date - prev) > $4
		ORDER BY gap DESC`, code, SourceNSEIndex, asOfIngest, minDays)
	if err != nil {
		return nil, fmt.Errorf("index gaps: %w", err)
	}
	defer rows.Close()
	var out []IndexGap
	for rows.Next() {
		var g IndexGap
		if err := rows.Scan(&g.After, &g.Before, &g.GapDays); err != nil {
			return nil, err
		}
		g.After = Day(g.After.Year(), g.After.Month(), g.After.Day())
		g.Before = Day(g.Before.Year(), g.Before.Month(), g.Before.Day())
		out = append(out, g)
	}
	return out, rows.Err()
}
