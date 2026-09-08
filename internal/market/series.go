package market

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// The return fence: M1's hard prerequisite, and the reason it is not a
// convention.
//
// After the entity change landed in M0 a company's series is continuous in
// IDENTITY and still discontinuous in LEVEL. nse-bhavcopy prices are
// unadjusted and the read-time adjustments layer does not exist, so a 12-1
// momentum signal crossing TATASTEEL's 2022 split reads -90% and looks like a
// number rather than an error. Before the roster was applied the two halves
// were disjoint symbol_ids and could not be differenced by accident; now they
// are one entity and they can. 334 of 2,141 entities carry a boundary.
//
// So until adjustments exists the only safe thing to do with a boundary is to
// REFUSE, and this file is where the refusal lives. Every return the engine
// computes goes through EntityReturns, and EntityReturns returns refusals as
// data the caller must account for rather than as a warning it can ignore.

// BoundaryGuardSessions is how many sessions BEFORE a succession boundary the
// fence also treats as unsafe, and it is not a round number picked for
// comfort.
//
// A boundary is the date the successor's first session fell on -- the ISIN
// change. The price break is the ex-date of the corporate action behind it,
// and the two are NOT the same day. Measured over all 444 links in the live
// roster on 2026-09-08, taking a >25% single-session move as the break:
//
//	at the boundary itself   167
//	1 session before it      262
//	2 sessions before it      14
//	3 sessions before it       0
//	4 sessions before it       1
//	5+ sessions before it      0
//
// Those sum to 444, which is every boundary in the roster accounted for
// exactly once -- each succession has one price break and it sits at the ISIN
// change or just before it. The majority case is the dangerous one: at
// TATASTEEL the ex-split session is 2022-07-28, closing 100.35 against the
// previous 959.40, while the ISIN only changes on 07-29. A fence that refused
// only windows SPANNING the boundary would price the 959.40 -> 100.35 step as
// a real -89.5% return, and it would do that for 59% of all boundaries.
//
// Five covers the measured maximum of four with one session of margin. Widening
// it is cheap -- it refuses a few more windows near a boundary and a 12-month
// momentum window has 250 sessions to land in -- and narrowing it is not, so
// this errs wide deliberately.
//
// What it does NOT cover: an adjustment INSIDE a single ISIN (a bonus, a
// rights issue, a demerger) produces the same kind of step with no boundary
// anywhere near it. The design measured 379 such steps against 241 succession
// steps and records them as permanently unfixed until adjustments exists.
// This fence is not evidence that a series is clean; it is evidence that a
// series is not broken by SUCCESSION.
const BoundaryGuardSessions = 5

// EndpointStaleDays bounds how far back an endpoint may reach for its close.
//
// "The last session on or before X" has no natural floor: an entity that
// stopped trading in 2014 would happily supply a 2014 close as the endpoint of
// a 2026 window, and the resulting "return" would be a decade of nothing. The
// bound makes that an explicit Absent with a reason instead of a number, and
// it also keeps the endpoint scan to two narrow date windows rather than the
// whole archive.
const EndpointStaleDays = 30

// EntityReturn is one entity's price return between two sessions, with the
// sessions it actually used. FromDate and ToDate are the last sessions at or
// before the requested dates, so a caller asking for a month end that fell on
// a holiday can see which session answered.
type EntityReturn struct {
	EntityID  int64
	FromDate  time.Time
	FromClose float64
	ToDate    time.Time
	ToClose   float64
	Return    float64
}

// BoundaryRefusal is one entity whose window touches a succession boundary's
// guard band, and therefore cannot be differenced while adjustments is
// unbuilt. It carries the boundary so the caller can report WHICH corporate
// action refused the name rather than dropping it silently.
type BoundaryRefusal struct {
	EntityID   int64
	FromDate   time.Time
	ToDate     time.Time
	GuardStart time.Time
	Boundary   EntityBoundary
}

func (r BoundaryRefusal) Error() string {
	return fmt.Sprintf(
		"entity %d: window %s..%s crosses the succession guard band %s..%s (%s -> %s); "+
			"nse-bhavcopy is unadjusted and the read-time adjustments layer does not exist, "+
			"so this return would be wrong by the split factor",
		r.EntityID,
		r.FromDate.Format(time.DateOnly), r.ToDate.Format(time.DateOnly),
		r.GuardStart.Format(time.DateOnly), r.Boundary.Date.Format(time.DateOnly),
		r.Boundary.PredecessorISIN, r.Boundary.SuccessorISIN)
}

// ReturnSet is the result of EntityReturns. Every requested entity appears in
// exactly one of the three maps, and EntityReturns errors rather than
// returning a set where that is not true.
//
// The three-way split is the whole ergonomic point. A caller cannot get at a
// priced return without walking past the refusals, and a name that vanished
// because its window straddled a split is distinguishable from one that
// vanished because it never traded. Returning refused names as an error would
// make momentum unrunnable -- some name is always near a split -- and dropping
// them silently is the failure this file exists to prevent.
type ReturnSet struct {
	Priced  map[int64]EntityReturn
	Refused map[int64]BoundaryRefusal
	Absent  map[int64]string
}

// Accounted reports an error unless every id appears exactly once across the
// three maps. EntityReturns calls it before returning; it is exported so a
// caller assembling its own set can make the same assertion.
func (rs ReturnSet) Accounted(ids []int64) error {
	seen := map[int64]int{}
	for id := range rs.Priced {
		seen[id]++
	}
	for id := range rs.Refused {
		seen[id]++
	}
	for id := range rs.Absent {
		seen[id]++
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var missing, extra []int64
	for id := range want {
		if seen[id] != 1 {
			missing = append(missing, id)
		}
	}
	for id := range seen {
		if !want[id] {
			extra = append(extra, id)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	return fmt.Errorf("return set does not account for every entity: %d unaccounted %v, %d unrequested %v",
		len(missing), missing, len(extra), extra)
}

type endpoint struct {
	date  time.Time
	close float64
	ok    bool
}

// EntityReturns prices the window (from, to] for each entity, refusing any
// entity whose window touches a succession boundary's guard band.
//
// Endpoints are the last session at or before each requested date, within
// EndpointStaleDays. Closes come from ONE source: the two sources disagree by
// the cumulative succession factor on every pre-boundary session (TATASTEEL on
// 2015-01-02 is eod2 41.10 and bhavcopy 410.75, exactly 1:10), so mixing them
// inside one return is the same bug this fence exists to catch, arriving by a
// different door.
//
// asOfIngest pins the bar versions, the entity map and the boundary set
// together. All three move on the same axis and pinning two of the three would
// leave a run reproducible in prices and not in identity.
func (s *Store) EntityReturns(ctx context.Context, source string, entityIDs []int64, from, to, asOfIngest time.Time) (ReturnSet, error) {
	rs := ReturnSet{
		Priced:  map[int64]EntityReturn{},
		Refused: map[int64]BoundaryRefusal{},
		Absent:  map[int64]string{},
	}
	ids := uniqueSorted(entityIDs)
	if len(ids) == 0 {
		return rs, nil
	}
	if !from.Before(to) {
		return rs, fmt.Errorf("entity returns: from %s must be before to %s",
			from.Format(time.DateOnly), to.Format(time.DateOnly))
	}

	fromEnd, toEnd, err := s.endpointCloses(ctx, source, ids, from, to, asOfIngest)
	if err != nil {
		return rs, err
	}
	boundaries, err := s.EntityBoundaries(ctx, ids, asOfIngest)
	if err != nil {
		return rs, err
	}
	guards, err := s.guardStarts(ctx, source, boundaries, asOfIngest)
	if err != nil {
		return rs, err
	}

	for _, id := range ids {
		f, t := fromEnd[id], toEnd[id]
		switch {
		case !f.ok && !t.ok:
			rs.Absent[id] = fmt.Sprintf("no %s session within %d days before either %s or %s",
				source, EndpointStaleDays, from.Format(time.DateOnly), to.Format(time.DateOnly))
			continue
		case !f.ok:
			rs.Absent[id] = fmt.Sprintf("no %s session within %d days before %s",
				source, EndpointStaleDays, from.Format(time.DateOnly))
			continue
		case !t.ok:
			rs.Absent[id] = fmt.Sprintf("no %s session within %d days before %s",
				source, EndpointStaleDays, to.Format(time.DateOnly))
			continue
		}
		if !f.date.Before(t.date) {
			rs.Absent[id] = fmt.Sprintf("both endpoints resolved to the same session %s",
				f.date.Format(time.DateOnly))
			continue
		}
		if refusal, refused := firstRefusal(id, f.date, t.date, boundaries[id], guards); refused {
			rs.Refused[id] = refusal
			continue
		}
		if f.close <= 0 {
			rs.Absent[id] = fmt.Sprintf("non-positive close %g on %s", f.close, f.date.Format(time.DateOnly))
			continue
		}
		rs.Priced[id] = EntityReturn{
			EntityID:  id,
			FromDate:  f.date,
			FromClose: f.close,
			ToDate:    t.date,
			ToClose:   t.close,
			Return:    t.close/f.close - 1,
		}
	}
	if err := rs.Accounted(ids); err != nil {
		return rs, err
	}
	return rs, nil
}

// firstRefusal returns the earliest boundary whose guard band the window
// touches.
//
// The window (fromDate, toDate] is unsafe when the price break could lie
// inside it, and the break lies somewhere in [guardStart, boundary]. So the
// test is: the window starts before the boundary AND reaches the band. Both
// endpoints strictly after the boundary is safe (post-split throughout), and
// both strictly before the band is safe (pre-split throughout).
func firstRefusal(id int64, fromDate, toDate time.Time, bs []EntityBoundary, guards map[time.Time]time.Time) (BoundaryRefusal, bool) {
	for _, b := range bs {
		guard, ok := guards[b.Date]
		if !ok {
			// No calendar behind the boundary to measure a band with. Fall
			// back to the boundary itself rather than to no guard at all.
			guard = b.Date
		}
		if fromDate.Before(b.Date) && !toDate.Before(guard) {
			return BoundaryRefusal{
				EntityID: id, FromDate: fromDate, ToDate: toDate,
				GuardStart: guard, Boundary: b,
			}, true
		}
	}
	return BoundaryRefusal{}, false
}

// endpointCloses resolves both endpoints for every entity in one query.
//
// It reads bars directly rather than through bars_latest, because that view
// takes the newest version of a row with no ingested_at bound and would make
// a pinned run answer with rows it could not have seen.
func (s *Store) endpointCloses(ctx context.Context, source string, ids []int64, from, to, asOfIngest time.Time) (map[int64]endpoint, map[int64]endpoint, error) {
	rows, err := s.pool.Query(ctx, `WITH `+entityMapCTE("$4")+`,
		sel AS (SELECT unnest($5::bigint[]) AS entity_id),
		mem AS (
			SELECT m.symbol_id, m.entity_id
			FROM entity_map m JOIN sel ON sel.entity_id = m.entity_id
		),
		latest AS (
			SELECT DISTINCT ON (b.symbol_id, b.date) b.symbol_id, b.date, b.close
			FROM bars b JOIN mem ON mem.symbol_id = b.symbol_id
			WHERE b.source = $1 AND b.ingested_at <= $4
			  AND ((b.date <= $2 AND b.date > ($2::date - $6::int))
			    OR (b.date <= $3 AND b.date > ($3::date - $6::int)))
			ORDER BY b.symbol_id, b.date, b.ingested_at DESC
		),
		ent AS (
			SELECT mem.entity_id, l.date, l.close
			FROM latest l JOIN mem ON mem.symbol_id = l.symbol_id
		),
		f AS (
			SELECT DISTINCT ON (entity_id) entity_id, date, close
			FROM ent WHERE date <= $2 ORDER BY entity_id, date DESC
		),
		t AS (
			SELECT DISTINCT ON (entity_id) entity_id, date, close
			FROM ent WHERE date <= $3 ORDER BY entity_id, date DESC
		)
		SELECT sel.entity_id, f.date, f.close::float8, t.date, t.close::float8
		FROM sel
		LEFT JOIN f ON f.entity_id = sel.entity_id
		LEFT JOIN t ON t.entity_id = sel.entity_id`,
		source, from, to, asOfIngest, ids, EndpointStaleDays)
	if err != nil {
		return nil, nil, fmt.Errorf("entity returns: endpoints: %w", err)
	}
	defer rows.Close()

	fromEnd := map[int64]endpoint{}
	toEnd := map[int64]endpoint{}
	for rows.Next() {
		var id int64
		var fd, td *time.Time
		var fc, tc *float64
		if err := rows.Scan(&id, &fd, &fc, &td, &tc); err != nil {
			return nil, nil, err
		}
		if fd != nil && fc != nil {
			fromEnd[id] = endpoint{date: Day(fd.Year(), fd.Month(), fd.Day()), close: *fc, ok: true}
		}
		if td != nil && tc != nil {
			toEnd[id] = endpoint{date: Day(td.Year(), td.Month(), td.Day()), close: *tc, ok: true}
		}
	}
	return fromEnd, toEnd, rows.Err()
}

// guardStarts maps each distinct boundary date to the first session of its
// guard band: the BoundaryGuardSessions'th session before it on the exchange
// calendar for this source.
//
// The calendar is the source's own sessions, not the entity's, and that
// direction is deliberate. An entity suspended for a week around its
// corporate action has fewer sessions of its own, so measuring the band on its
// own trading would place guardStart closer to the boundary and narrow the
// fence at exactly the moment it is most needed.
func (s *Store) guardStarts(ctx context.Context, source string, boundaries map[int64][]EntityBoundary, asOfIngest time.Time) (map[time.Time]time.Time, error) {
	out := map[time.Time]time.Time{}
	seen := map[time.Time]bool{}
	var dates []time.Time
	for _, bs := range boundaries {
		for _, b := range bs {
			if !seen[b.Date] {
				seen[b.Date] = true
				dates = append(dates, b.Date)
			}
		}
	}
	if len(dates) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT b.boundary, min(d.date)
		FROM unnest($3::date[]) AS b(boundary)
		JOIN LATERAL (
			SELECT DISTINCT date FROM bars
			WHERE source = $1 AND ingested_at <= $2
			  AND date < b.boundary AND date > (b.boundary - $5::int)
			ORDER BY date DESC LIMIT $4
		) d ON true
		GROUP BY b.boundary`,
		source, asOfIngest, dates, BoundaryGuardSessions, guardScanDays)
	if err != nil {
		return nil, fmt.Errorf("entity returns: guard band: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var boundary, start time.Time
		if err := rows.Scan(&boundary, &start); err != nil {
			return nil, err
		}
		out[Day(boundary.Year(), boundary.Month(), boundary.Day())] =
			Day(start.Year(), start.Month(), start.Day())
	}
	return out, rows.Err()
}

// guardScanDays bounds the calendar lookup behind each boundary. Five sessions
// span at most a fortnight even across a long exchange holiday, so 30 calendar
// days finds them with room to spare while keeping the scan an index range per
// boundary rather than a pass over every session in the archive.
const guardScanDays = 30

func uniqueSorted(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
