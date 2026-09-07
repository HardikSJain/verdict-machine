package market

import (
	"context"
	"fmt"
	"time"
)

// UniverseMember is one row of a point-in-time universe ranking.
type UniverseMember struct {
	SymbolID       int64
	ISIN           string
	Ticker         string
	MedianTurnover float64 // rupees per day, median over the lookback window
	DaysPresent    int
}

// Universe is one point-in-time universe: the ranked members, and the number
// of sessions they were actually ranked over.
type Universe struct {
	// Sessions is how many sessions the window really held. It is at most
	// lookbackDays, and smaller whenever the store holds less history than
	// that -- mid-backfill, or near the start of the archive. A caller that
	// asked for six months and got two sessions must be able to tell, because
	// a two-session median is not a liquidity filter and the returned members
	// look identical either way.
	Sessions int
	Members  []UniverseMember
}

// windowCTE defines the point-in-time window. The ranking and the realised
// session count are read from this one definition in one statement, so the
// count can never describe a different window from the one that was ranked --
// not a different definition of it, and not a different snapshot of it. Two
// statements would be two snapshots, and this project ingests concurrently
// with reads, so a bar landing between them would make the reported window a
// description of a window nothing was ranked over.
//
// $1 source, $2 asOf, $3 since, $4 asOfIngest, $5 lookbackDays.
const windowCTE = `
	WITH latest AS (
		SELECT DISTINCT ON (symbol_id, date) symbol_id, date, series, turnover, close, volume
		FROM bars
		WHERE source = $1 AND date > $3 AND date <= $2 AND ingested_at <= $4
		ORDER BY symbol_id, date, ingested_at DESC
	), days AS (
		SELECT DISTINCT date FROM latest ORDER BY date DESC LIMIT $5
	)`

// UniverseAsOf ranks series-EQ symbols by median daily rupee turnover over the
// last lookbackDays sessions ending at asOf, using only bar versions ingested
// at or before asOfIngest, and returns the top n. A symbol must be present on
// at least 80% of the window's sessions. Because it is computed from bars
// alone, a name that traded then and was delisted since is still in the
// universe for that date; no survivorship bias.
//
// When the store holds fewer than lookbackDays sessions the window silently
// shortens to what is there -- the 80% presence rule scales with it too -- so
// the realised session count comes back in Universe.Sessions rather than
// leaving a two-session ranking indistinguishable from a six-month one.
//
// Each member is labelled with the ticker its symbol carried on asOf, not
// today's ticker; see symbolLabelLateral.
//
// For sources without a turnover column (eod2), close * volume stands in.
func (s *Store) UniverseAsOf(ctx context.Context, source string, asOf time.Time, lookbackDays, n int, asOfIngest time.Time) (Universe, error) {
	if lookbackDays <= 0 || n <= 0 {
		return Universe{}, fmt.Errorf("universe: lookbackDays and n must be positive")
	}
	if source != SourceBhavcopy && source != SourceEod2 {
		return Universe{}, fmt.Errorf("universe: unknown source %q (want %q or %q)", source, SourceBhavcopy, SourceEod2)
	}
	// Bound the scan: lookbackDays sessions never span more than 2x that in calendar days plus holidays.
	since := asOf.AddDate(0, 0, -(lookbackDays*2 + 14))

	// The session count rides along as a column of the ranking rather than
	// coming from a second query: one statement is one snapshot, and the
	// window a caller is told about is then necessarily the window its members
	// were ranked over. `w` is a single row, so cross-joining it repeats the
	// count on every member and changes nothing else.
	//
	// It is a LEFT JOIN out of `w` and not into it because a universe can
	// legitimately rank nobody -- a window whose sessions hold no EQ series,
	// or where no symbol clears the 80% presence rule -- and that caller needs
	// the realised window most of all, to tell "the window is real and empty"
	// from "there is no window here". So `w` is the driving side and produces
	// its row either way; the member columns come back NULL when `ranked` is
	// empty, and that one row is skipped below.
	rows, err := s.pool.Query(ctx, windowCTE+`, window_rows AS (
			SELECT l.symbol_id, COALESCE(l.turnover, l.close * l.volume)::float8 AS turnover
			FROM latest l JOIN days USING (date)
			WHERE l.series = 'EQ'
		), ranked AS (
			SELECT symbol_id,
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY turnover)::float8 AS median_turnover,
			       count(*)::int AS days_present
			FROM window_rows
			GROUP BY symbol_id
			HAVING count(*) >= ceil(0.8 * (SELECT count(*) FROM days))
		)
		SELECT w.sessions, r.symbol_id, sym.isin, sym.ticker, r.median_turnover, r.days_present
		FROM (SELECT count(*)::int AS sessions FROM days) w
		LEFT JOIN ranked r ON true
		LEFT JOIN LATERAL `+symbolLabelLateral("r.symbol_id", "$2", "$4")+` sym ON true
		ORDER BY r.median_turnover DESC NULLS LAST, sym.ticker
		LIMIT $6`, source, asOf, since, asOfIngest, lookbackDays, n)
	if err != nil {
		return Universe{}, fmt.Errorf("universe: %w", err)
	}
	defer rows.Close()
	var u Universe
	for rows.Next() {
		var m UniverseMember
		// Nullable only for the no-members row described above; when `ranked`
		// has anything at all, every one of these is present on every row.
		var symbolID *int64
		var isin, ticker *string
		var median *float64
		var daysPresent *int
		if err := rows.Scan(&u.Sessions, &symbolID, &isin, &ticker, &median, &daysPresent); err != nil {
			return Universe{}, err
		}
		if symbolID == nil {
			continue
		}
		m.SymbolID, m.ISIN, m.Ticker = *symbolID, *isin, *ticker
		m.MedianTurnover, m.DaysPresent = *median, *daysPresent
		u.Members = append(u.Members, m)
	}
	if err := rows.Err(); err != nil {
		return Universe{}, err
	}
	return u, nil
}
