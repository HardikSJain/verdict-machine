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

// UniverseAsOf ranks series-EQ symbols by median daily rupee turnover over the
// last lookbackDays sessions ending at asOf, using only bar versions ingested
// at or before asOfIngest, and returns the top n. A symbol must be present on
// at least 80% of the window's sessions. Because it is computed from bars
// alone, a name that traded then and was delisted since is still in the
// universe for that date; no survivorship bias.
//
// For sources without a turnover column (eod2), close * volume stands in.
func (s *Store) UniverseAsOf(ctx context.Context, source string, asOf time.Time, lookbackDays, n int, asOfIngest time.Time) ([]UniverseMember, error) {
	if lookbackDays <= 0 || n <= 0 {
		return nil, fmt.Errorf("universe: lookbackDays and n must be positive")
	}
	// Bound the scan: lookbackDays sessions never span more than 2x that in calendar days plus holidays.
	since := asOf.AddDate(0, 0, -(lookbackDays*2 + 14))
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (symbol_id, date) symbol_id, date, series, turnover, close, volume
			FROM bars
			WHERE source = $1 AND date > $6 AND date <= $2 AND ingested_at <= $5
			ORDER BY symbol_id, date, ingested_at DESC
		), days AS (
			SELECT DISTINCT date FROM latest ORDER BY date DESC LIMIT $3
		), window_rows AS (
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
		SELECT r.symbol_id, sym.isin, sym.ticker, r.median_turnover, r.days_present
		FROM ranked r
		JOIN LATERAL (
			SELECT isin, ticker FROM symbols WHERE symbol_id = r.symbol_id AND ingested_at <= $5
			ORDER BY ingested_at DESC LIMIT 1
		) sym ON true
		ORDER BY r.median_turnover DESC, sym.ticker
		LIMIT $4`, source, asOf, lookbackDays, n, asOfIngest, since)
	if err != nil {
		return nil, fmt.Errorf("universe: %w", err)
	}
	defer rows.Close()
	var out []UniverseMember
	for rows.Next() {
		var m UniverseMember
		if err := rows.Scan(&m.SymbolID, &m.ISIN, &m.Ticker, &m.MedianTurnover, &m.DaysPresent); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
