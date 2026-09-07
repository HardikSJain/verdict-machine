package market

import (
	"context"
	"fmt"
	"time"
)

// UniverseMember is one row of a point-in-time universe ranking. It names an
// ENTITY -- a company -- not a physical symbol_id, because NSE reissues an
// ISIN on a face-value split and neither half of a split company can clear
// the 80% presence gate for roughly six months around the change.
type UniverseMember struct {
	// EntityID is the canonical company and the key a caller must join back
	// to bars on -- through entity_map_at(the same asOfIngest that produced
	// this row), never through entity_map_now. Resolving a pinned read's
	// entity ids at now() is the replay leak: the row set is unchanged and
	// only the resolution moves, so nothing downstream can notice.
	EntityID int64
	// SymbolID is the member whose label is in force on asOf: post-split for
	// a present-day query, pre-split for a 2015 one. It is NOT a safe join
	// key for the window -- see Fragments.
	SymbolID       int64
	ISIN           string
	Ticker         string
	MedianTurnover float64 // rupees per day, median over the lookback window
	// DaysPresent counts distinct SESSIONS the entity traded on, not rows.
	DaysPresent int
	// Fragments is how many physical symbol_ids contributed to this window.
	// Greater than one means joining SymbolID straight back to bars.symbol_id
	// would silently miss part of the window.
	Fragments int
	// LastBreak is the most recent succession boundary at or before asOf, if
	// any. A return computed across it on unadjusted bhavcopy prices is wrong
	// by the split factor; see EntityBoundaries.
	LastBreak *time.Time
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
	// RequiredLastSession reports which membership rule actually produced
	// these members: true when a symbol had to trade on the window's final
	// session to qualify (the default), false when AllowStaleMembers relaxed
	// that. A caller cannot otherwise tell "the option ran" from "the option
	// silently did nothing".
	RequiredLastSession bool
	Members             []UniverseMember
}

// universeOptions holds UniverseAsOf's optional membership-rule overrides.
type universeOptions struct {
	allowStale bool
}

// UniverseOption customizes UniverseAsOf's membership rule.
type UniverseOption func(*universeOptions)

// AllowStaleMembers keeps a name whose most recent bar predates the window's
// final session. The default is to drop it: the strategy this universe feeds
// rebalances at the open on the prior close's signals, so a name absent from
// the window's last session has no close to signal on and no price to size
// from -- it is not tradeable that morning, whatever its turnover looked like
// while it still traded. Pass this only for a study that deliberately wants
// halted and suspended names in the panel.
func AllowStaleMembers() UniverseOption {
	return func(o *universeOptions) { o.allowStale = true }
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

// UniverseAsOf ranks series-EQ ENTITIES by median daily rupee turnover over
// the last lookbackDays sessions ending at asOf, using only bar versions
// ingested at or before asOfIngest, and returns the top n. An entity must be
// present on at least 80% of the window's sessions AND, by default, must have
// traded on the window's final session -- see AllowStaleMembers. Because it
// is computed from bars alone, a name that traded then and was delisted since
// is still in the universe for that date; no survivorship bias. The liveness
// rule is narrower than survivorship: it excludes a name that had already
// gone dark by asOf (a succession that left the old ISIN untraded, a
// suspension), not one that trades fine on asOf and is delisted only later.
//
// Grouping on entity rather than symbol_id is what this whole layer exists
// for. NSE reissues an ISIN on a face-value split, so a split company is two
// disjoint symbol_ids and NEITHER can clear the 80% gate for roughly six
// months around the change: measured on the live store at asOf 2022-10-31
// with a 125-session lookback, TATASTEEL's two halves hold 62 and 63 of 125
// sessions where 100 are needed, so India's 28th most traded name is absent
// from its own point-in-time universe for that entire window and then
// reappears as a stranger with no history. 491 tickers in the live database
// already hold more than one symbol_id and 101 of them are in today's top 500
// by median turnover. Presence is therefore counted in distinct SESSIONS
// across the entity's members, not in rows.
//
// DaysPresent is a semantic change from the per-symbol version and old and
// new values are not guaranteed to match. When an entity's members overlap on
// a session -- which is only possible through a wrong link -- this returns an
// error rather than a ranking; see the member_rows check below.
//
// When the store holds fewer than lookbackDays sessions the window silently
// shortens to what is there -- the 80% presence rule scales with it too -- so
// the realised session count comes back in Universe.Sessions rather than
// leaving a two-session ranking indistinguishable from a six-month one.
//
// Each member is labelled with the ISIN and ticker its ENTITY carried on asOf
// -- the member in force on that date, then that member's label in force on
// that date; see entityMemberCTE and symbolLabelLateral.
//
// For sources without a turnover column (eod2), close * volume stands in.
func (s *Store) UniverseAsOf(ctx context.Context, source string, asOf time.Time, lookbackDays, n int, asOfIngest time.Time, opts ...UniverseOption) (Universe, error) {
	if lookbackDays <= 0 || n <= 0 {
		return Universe{}, fmt.Errorf("universe: lookbackDays and n must be positive")
	}
	if source != SourceBhavcopy && source != SourceEod2 {
		return Universe{}, fmt.Errorf("universe: unknown source %q (want %q or %q)", source, SourceBhavcopy, SourceEod2)
	}
	var cfg universeOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	requireLastSession := !cfg.allowStale
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
	// window_rows carries `date` (not just symbol_id/turnover) purely so the
	// liveness term below has something to compare against max(days.date);
	// the guard has to live in this CTE's HAVING because that is where the
	// per-symbol aggregation happens, and it is the easiest thing in this
	// query to apply to the wrong one.
	//
	// $7 is requireLastSession, not "allowStale": when it is false the OR
	// short-circuits and the term is a no-op; when true, bool_or(...) demands
	// that the symbol traded on the window's very last session. Getting the
	// polarity backwards here would silently invert default and opt-in.
	rows, err := s.pool.Query(ctx, windowCTE+`, `+
		entityMapCTE("$4")+`, `+
		entityMemberCTE("$2", "$4")+`, `+
		entityLabelCTE("$2", "$4")+`, window_rows AS (
			SELECT m.entity_id, l.symbol_id, l.date,
			       COALESCE(l.turnover, l.close * l.volume)::float8 AS turnover
			FROM latest l
			JOIN days USING (date)
			JOIN entity_map m ON m.symbol_id = l.symbol_id
			WHERE l.series = 'EQ'
		), ranked AS (
			SELECT entity_id,
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY turnover)::float8 AS median_turnover,
			       count(DISTINCT date)::int      AS days_present,
			       count(*)::int                  AS member_rows,
			       count(DISTINCT symbol_id)::int AS fragments
			FROM window_rows
			GROUP BY entity_id
			HAVING count(DISTINCT date) >= ceil(0.8 * (SELECT count(*) FROM days))
			   AND ($7 IS FALSE OR bool_or(date = (SELECT max(date) FROM days)))
		), entity_break AS (
			SELECT m.entity_id, max(k.boundary) AS last_break
			FROM entity_map m
			JOIN LATERAL (
				SELECT boundary FROM symbol_links
				WHERE symbol_id = m.symbol_id AND ingested_at <= $4
				ORDER BY ingested_at DESC LIMIT 1
			) k ON true
			WHERE k.boundary IS NOT NULL AND k.boundary <= $2
			GROUP BY m.entity_id
		)
		SELECT w.sessions, r.entity_id, el.symbol_id, el.isin, el.ticker,
		       r.median_turnover, r.days_present, r.member_rows, r.fragments, eb.last_break
		FROM (SELECT count(*)::int AS sessions FROM days) w
		LEFT JOIN ranked r ON true
		LEFT JOIN entity_label el ON el.entity_id = r.entity_id
		LEFT JOIN entity_break eb ON eb.entity_id = r.entity_id
		ORDER BY r.median_turnover DESC NULLS LAST, el.ticker
		LIMIT $6`, source, asOf, since, asOfIngest, lookbackDays, n, requireLastSession)
	if err != nil {
		return Universe{}, fmt.Errorf("universe: %w", err)
	}
	defer rows.Close()
	var u Universe
	for rows.Next() {
		var m UniverseMember
		// Nullable only for the no-members row described above; when `ranked`
		// has anything at all, every one of these is present on every row.
		var entityID, symbolID *int64
		var isin, ticker *string
		var median *float64
		var daysPresent, memberRows, fragments *int
		var lastBreak *time.Time
		if err := rows.Scan(&u.Sessions, &entityID, &symbolID, &isin, &ticker, &median,
			&daysPresent, &memberRows, &fragments, &lastBreak); err != nil {
			return Universe{}, err
		}
		if entityID == nil {
			continue
		}
		// `latest` is DISTINCT ON (symbol_id, date), so within one entity
		// member_rows > days_present says two members traded the same
		// session: two genuinely different companies merged by a bad link,
		// in a store that cannot delete. Absorbed silently it would return a
		// median over two companies' turnovers and inflate the entity past
		// the 80% gate, with no error anywhere. This is the only overlap
		// detector on the read M1 actually calls -- BarsForDate has zero
		// non-test callers -- so it is the one that has to fire.
		if *memberRows > *daysPresent {
			return Universe{}, fmt.Errorf(
				"universe: entity %d has %d member rows over %d distinct sessions in the %d-session %s window "+
					"ending %s; two members of one entity traded the same session, so the link joining them "+
					"merges two different companies -- run `verdict entities check` before trusting this universe",
				*entityID, *memberRows, *daysPresent, u.Sessions, source, asOf.Format("2006-01-02"))
		}
		if symbolID == nil || isin == nil || ticker == nil {
			return Universe{}, fmt.Errorf(
				"universe: entity %d ranked but has no label in force on %s; entity_member lost a member",
				*entityID, asOf.Format("2006-01-02"))
		}
		m.EntityID, m.SymbolID, m.ISIN, m.Ticker = *entityID, *symbolID, *isin, *ticker
		m.MedianTurnover, m.DaysPresent, m.Fragments = *median, *daysPresent, *fragments
		if lastBreak != nil {
			d := Day(lastBreak.Year(), lastBreak.Month(), lastBreak.Day())
			m.LastBreak = &d
		}
		u.Members = append(u.Members, m)
	}
	if err := rows.Err(); err != nil {
		return Universe{}, err
	}
	u.RequiredLastSession = requireLastSession
	return u, nil
}
