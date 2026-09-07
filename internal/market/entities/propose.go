package entities

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// The candidate generator and the gates, run against the store.
//
// propose is READ-ONLY and it is the only part of this package that reads
// bars. It generates candidates, measures every gate, writes the roster of
// what passed and a review file of everything it looked at, and writes
// nothing to the store.
//
// It also carries no licence to run mid-backfill, and that is not a style
// note: G4 counts sessions out of bars, and bars completeness is an
// operational property of a resumable HTTP fetcher rather than an invariant.
// A real NSE session the archive has not fetched reads as "zero sessions
// between", and the 2017 Tube Investments demerger -- disjoint, same issuer,
// serial increasing, 31 live sessions in its gap -- then passes every gate
// and merges two economically different securities. G4a is the mechanical
// enforcement: propose refuses to emit a single auto-accept line while the
// archive span has an unsettled hole in it.

// Gate names, in the order they are evaluated and reported.
const (
	G0  = "G0 well-formed"
	G1  = "G1 issuer identity"
	G2  = "G2 serial monotone"
	G3  = "G3 disjoint"
	G4  = "G4 adjacent"
	G4a = "G4a settled calendar"
	G4b = "G4b calendar cap"
	G5  = "G5 path not graph"
	G6  = "G6 boundary continuity"
)

// GateOrder is every gate, in evaluation order, so a report can list them all
// including the ones nothing failed.
var GateOrder = []string{G0, G1, G2, G3, G4, G4a, G4b, G5, G6}

// ist is India Standard Time, the clock NSE's sessions and the evening
// bhavcopy publish both run on. It is duplicated from bhavcopy.ist rather
// than exported from there, because the settlement horizon below has to agree
// with backfill's noFileSettled and the two should be read together.
var ist = time.FixedZone("IST", 5*60*60+30*60)

// settleLag mirrors bhavcopy.noFileSettleLag: how long after a session date
// has ended (midnight IST) a 404 stops meaning "NSE has not published this
// yet" and starts meaning "there was no session".
const settleLag = 24 * time.Hour

// symbolFacts is everything propose knows about one physical symbol.
type symbolFacts struct {
	SymbolID    int64
	ISIN        string
	FirstDate   time.Time
	LastDate    time.Time
	Bars        int
	FirstClose  float64
	LastClose   float64
	FirstTurn   *float64
	LastTurn    *float64
	FirstDeliv  *int64
	LastDeliv   *int64
	FirstTicker string
	LastTicker  string
	Eod2Bars    int
	Eod2First   *time.Time
}

// Candidate is one ordered pair the generator produced, with every gate
// result and the boundary-continuity report attached.
type Candidate struct {
	Generator      string     `json:"generator"`
	Predecessor    string     `json:"predecessor"`
	Successor      string     `json:"successor"`
	PredecessorID  int64      `json:"predecessor_symbol_id"`
	SuccessorID    int64      `json:"successor_symbol_id"`
	TickerBefore   string     `json:"ticker_before"`
	TickerAfter    string     `json:"ticker_after"`
	PredecessorSpn [2]string  `json:"predecessor_span"`
	SuccessorSpn   [2]string  `json:"successor_span"`
	OverlapDates   int        `json:"overlap_dates"`
	BoundaryClose  [2]float64 `json:"boundary_close"`
	TurnoverRatio  *float64   `json:"boundary_turnover_ratio"`
	DeliveryRatio  *float64   `json:"boundary_delivery_ratio"`

	Gates             Gates             `json:"gates"`
	EvidenceNotGating EvidenceNotGating `json:"evidence_not_gating"`

	Disposition string   `json:"disposition"`
	FailedGates []string `json:"failed_gates,omitempty"`
}

// Accepted reports whether every gate held.
func (c *Candidate) Accepted() bool { return len(c.FailedGates) == 0 }

// Counts is what propose measured, for the operator and for the report.
type Counts struct {
	Symbols          int
	Candidates       int
	Accepted         int
	Quarantined      int
	FirstFailure     map[string]int // first gate that failed -> candidates
	AnyFailure       map[string]int // gate -> candidates it rejected
	Sessions         int
	ArchiveFrom      time.Time
	ArchiveTo        time.Time
	ArchiveUnsettled int
	ArchivePending   int
}

// ProposeOptions configures one read-only run.
type ProposeOptions struct {
	// AsOfIngest pins every read, so a propose run is reproducible: the
	// candidates are a function of the store as it was at one moment.
	AsOfIngest time.Time
	RatifiedBy string
	RatifiedAt string
}

// Propose generates candidates, runs the gates and returns the roster of what
// passed, the review record of everything it looked at, and the counts.
//
// It never writes to the store.
func Propose(ctx context.Context, pool *pgxpool.Pool, opts ProposeOptions) (*Roster, []Candidate, *Counts, error) {
	if opts.AsOfIngest.IsZero() {
		opts.AsOfIngest = time.Now()
	}
	facts, err := loadSymbolFacts(ctx, pool, opts.AsOfIngest)
	if err != nil {
		return nil, nil, nil, err
	}
	sessions, err := loadSessions(ctx, pool, opts.AsOfIngest)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(sessions) == 0 {
		return nil, nil, nil, fmt.Errorf("propose: the store holds no %s bars; there is nothing to propose", market.SourceBhavcopy)
	}
	settled, lastFetch, err := loadSettlement(ctx, pool, opts.AsOfIngest)
	if err != nil {
		return nil, nil, nil, err
	}

	counts := &Counts{
		Symbols:      len(facts),
		Sessions:     len(sessions),
		ArchiveFrom:  sessions[0],
		ArchiveTo:    sessions[len(sessions)-1],
		FirstFailure: map[string]int{},
		AnyFailure:   map[string]int{},
	}
	holes, pending := archiveHoles(counts.ArchiveFrom, counts.ArchiveTo, settled, lastFetch)
	counts.ArchiveUnsettled, counts.ArchivePending = len(holes), len(pending)
	if len(holes) > 0 {
		return nil, nil, counts, fmt.Errorf(
			"propose: %d date(s) inside the archive span %s..%s are unsettled in ingest_log (neither 'ok' nor 'no-file'), starting %s; "+
				"G4 counts sessions out of bars, so a session the store has not fetched reads as \"zero sessions between\" and a demerger "+
				"passes every gate -- re-run `verdict backfill` and propose again",
			len(holes), counts.ArchiveFrom.Format("2006-01-02"), counts.ArchiveTo.Format("2006-01-02"), formatDates(holes, 10))
	}

	candidates := generate(facts)
	counts.Candidates = len(candidates)
	if err := measureOverlaps(ctx, pool, opts.AsOfIngest, candidates); err != nil {
		return nil, nil, counts, err
	}
	for _, c := range candidates {
		gate(c, sessions, settled, counts.ArchiveUnsettled)
	}
	applyPathGate(candidates)

	r := &Roster{Version: RosterVersion}
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Accepted() {
			c.Disposition = "accepted"
			counts.Accepted++
			r.Links = append(r.Links, c.link(opts))
		} else {
			c.Disposition = "quarantined"
			counts.Quarantined++
			counts.FirstFailure[c.FailedGates[0]]++
			for _, g := range c.FailedGates {
				counts.AnyFailure[g]++
			}
		}
		out = append(out, *c)
	}
	r.Sort()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Predecessor != out[j].Predecessor {
			return out[i].Predecessor < out[j].Predecessor
		}
		return out[i].Successor < out[j].Successor
	})
	return r, out, counts, r.Validate()
}

func (c *Candidate) link(opts ProposeOptions) Link {
	return Link{
		Reason:            ReasonSuccession,
		Predecessor:       c.Predecessor,
		Successor:         c.Successor,
		TickerAtBoundary:  c.TickerAfter,
		EffectiveFrom:     c.SuccessorSpn[0],
		Gates:             c.Gates,
		EvidenceNotGating: c.EvidenceNotGating,
		RatifiedBy:        opts.RatifiedBy,
		RatifiedAt:        opts.RatifiedAt,
	}
}

// generate produces the ordered candidate pairs, keyed on the ISIN and not on
// the ticker.
//
// NSDL's scheme means the identifier names its own issuer, so same-left(9) is
// the primary key: it finds 121 pairs a ticker-keyed generator misses
// entirely -- successions where the ISIN and the ticker changed on the same
// session, so neither the ISIN rule nor the already-solved rename rule sees
// them (CADILAHC -> ZYDUSLIFE, AMARAJABAT -> ARE&M, MINDAIND -> UNOMINDA).
//
// The ticker-keyed pass exists only to SURFACE the fund-unit cases for
// quarantine. On an AMC transfer the whole ISIN changes (NIFTYBEES
// INF732E01011 -> INF204KB14I2), so no prefix rule can see them; they fail G0
// and G1 by construction and land in the review file, which is exactly where
// design 5.4 puts them.
//
// Within a group the members are ordered by their spans and only CONSECUTIVE
// members are paired, which is what makes the generated graph a set of paths
// rather than a mesh: a four-ISIN chain produces three pairs, not six.
func generate(facts map[int64]*symbolFacts) []*Candidate {
	byPrefix := map[string][]*symbolFacts{}
	byTicker := map[string][]*symbolFacts{}
	for _, f := range facts {
		if IsEquityISIN(f.ISIN) {
			byPrefix[IssuerPrefix(f.ISIN)] = append(byPrefix[IssuerPrefix(f.ISIN)], f)
		}
		byTicker[f.LastTicker] = append(byTicker[f.LastTicker], f)
	}
	seen := map[[2]int64]bool{}
	var out []*Candidate
	add := func(generator string, groups map[string][]*symbolFacts) {
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			g := groups[k]
			if len(g) < 2 {
				continue
			}
			sort.Slice(g, func(i, j int) bool {
				if !g[i].FirstDate.Equal(g[j].FirstDate) {
					return g[i].FirstDate.Before(g[j].FirstDate)
				}
				return g[i].SymbolID < g[j].SymbolID
			})
			for i := 0; i+1 < len(g); i++ {
				key := [2]int64{g[i].SymbolID, g[i+1].SymbolID}
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, newCandidate(generator, g[i], g[i+1]))
			}
		}
	}
	add("issuer-prefix", byPrefix)
	add("ticker", byTicker)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Predecessor != out[j].Predecessor {
			return out[i].Predecessor < out[j].Predecessor
		}
		return out[i].Successor < out[j].Successor
	})
	return out
}

func newCandidate(generator string, p, s *symbolFacts) *Candidate {
	c := &Candidate{
		Generator:      generator,
		Predecessor:    p.ISIN,
		Successor:      s.ISIN,
		PredecessorID:  p.SymbolID,
		SuccessorID:    s.SymbolID,
		TickerBefore:   p.LastTicker,
		TickerAfter:    s.FirstTicker,
		PredecessorSpn: [2]string{day(p.FirstDate), day(p.LastDate)},
		SuccessorSpn:   [2]string{day(s.FirstDate), day(s.LastDate)},
		BoundaryClose:  [2]float64{p.LastClose, s.FirstClose},
		TurnoverRatio:  ratio(s.FirstTurn, p.LastTurn),
		DeliveryRatio:  intRatio(s.FirstDeliv, p.LastDeliv),
	}
	c.Gates = Gates{
		CheckDigit:         CheckDigitOK(p.ISIN) && CheckDigitOK(s.ISIN),
		IssuerPrefix:       IssuerPrefix(p.ISIN),
		PredecessorLastBar: day(p.LastDate),
		SuccessorFirstBar:  day(s.FirstDate),
		PredecessorBars:    p.Bars,
		SuccessorBars:      s.Bars,
	}
	if ps, ok := IssueSerial(p.ISIN); ok {
		if ss, ok2 := IssueSerial(s.ISIN); ok2 {
			c.Gates.Serial = fmt.Sprintf("%02d->%02d", ps, ss)
		}
	}
	if p.LastClose != 0 {
		c.Gates.BoundaryCloseRatio = round4(s.FirstClose / p.LastClose)
		c.Gates.BoundaryRatioMatches = MatchRatioBand(c.Gates.BoundaryCloseRatio)
	}
	// eod2 is recorded and gates nothing. eod2.LoadDir maps a whole CSV to
	// the ticker's CURRENT ISIN, so its continuity is ticker-based: for a
	// genuine ticker reuse it already contains the splice being guarded
	// against and would corroborate a wrong merge exactly as loudly as a real
	// split. It also derives from the same NSE files, so it is not an
	// independent source, and every line says so.
	c.EvidenceNotGating = EvidenceNotGating{
		Eod2Independent:         false,
		Eod2PredecessorBars:     p.Eod2Bars,
		Eod2ISIN2HistAgrees:     s.Eod2First != nil && !s.Eod2First.After(p.LastDate),
		BoundaryTickerUnchanged: p.LastTicker == s.FirstTicker,
	}
	return c
}

// gate evaluates G0-G4b and G6 for one candidate. G5 is a property of the
// accepted SET and is applied afterwards by applyPathGate.
func gate(c *Candidate, sessions []time.Time, settled map[time.Time]bool, archiveUnsettled int) {
	fail := func(g string) { c.FailedGates = append(c.FailedGates, g) }

	if !IsEquityISIN(c.Predecessor) || !IsEquityISIN(c.Successor) ||
		!WellFormedISIN(c.Predecessor) || !WellFormedISIN(c.Successor) {
		fail(G0)
	}
	if IssuerPrefix(c.Predecessor) != IssuerPrefix(c.Successor) {
		fail(G1)
	}
	ps, pok := IssueSerial(c.Predecessor)
	ss, sok := IssueSerial(c.Successor)
	if !pok || !sok || ss <= ps {
		fail(G2)
	}

	predLast, _ := parseDay(c.Gates.PredecessorLastBar)
	succFirst, _ := parseDay(c.Gates.SuccessorFirstBar)
	if !predLast.Before(succFirst) || c.OverlapDates != 0 {
		fail(G3)
	}

	// G4: live sessions strictly inside the gap. This is the gate that kills
	// demergers, and it earns its keep on INE149A01025 -> INE149A01033, the
	// 2017 Tube Investments demerger: disjoint, same issuer, serial
	// increasing, and only the 31 live sessions in its gap stop it.
	c.Gates.SessionsBetween = countBetween(sessions, predLast, succFirst)
	if c.Gates.SessionsBetween != 0 {
		fail(G4)
	}

	// G4a: every calendar date in the gap settled, plus the archive-wide
	// refusal already applied by the caller.
	c.Gates.GapDatesAllSettled = true
	for d := predLast.AddDate(0, 0, 1); d.Before(succFirst); d = d.AddDate(0, 0, 1) {
		if !settled[d] {
			c.Gates.GapDatesAllSettled = false
			break
		}
	}
	c.Gates.ArchiveUnsettled = archiveUnsettled
	if !c.Gates.GapDatesAllSettled || c.Gates.ArchiveUnsettled != 0 {
		fail(G4a)
	}

	c.Gates.CalendarDaysBetween = int(succFirst.Sub(predLast).Hours() / 24)
	if c.Gates.CalendarDaysBetween < 1 || c.Gates.CalendarDaysBetween > maxCalendarDaysBetween {
		fail(G4b)
	}

	if c.Gates.BoundaryRatioMatches == "" {
		fail(G6)
	}
	sortGates(c.FailedGates)
}

// applyPathGate is G5. The generator pairs only consecutive members of a
// group, so a single generator cannot produce a branch -- but two generators
// run over the same symbols, and a hand-edited roster is not bound by either,
// so the shape is checked rather than assumed. Every candidate in a conflict
// is quarantined, not arbitrated: choosing between two claims on one
// successor is exactly the judgement this design refuses to make by machine.
func applyPathGate(candidates []*Candidate) {
	successors := map[string][]*Candidate{}
	predecessors := map[string][]*Candidate{}
	for _, c := range candidates {
		if !c.Accepted() {
			continue
		}
		successors[c.Successor] = append(successors[c.Successor], c)
		predecessors[c.Predecessor] = append(predecessors[c.Predecessor], c)
	}
	for _, group := range []map[string][]*Candidate{successors, predecessors} {
		for _, cs := range group {
			if len(cs) < 2 {
				continue
			}
			for _, c := range cs {
				c.FailedGates = append(c.FailedGates, G5)
				sortGates(c.FailedGates)
			}
		}
	}
}

func sortGates(gates []string) {
	rank := func(g string) int {
		for i, name := range GateOrder {
			if name == g {
				return i
			}
		}
		return len(GateOrder)
	}
	sort.SliceStable(gates, func(i, j int) bool { return rank(gates[i]) < rank(gates[j]) })
}

// countBetween counts sessions strictly between two dates.
func countBetween(sessions []time.Time, from, to time.Time) int {
	lo := sort.Search(len(sessions), func(i int) bool { return sessions[i].After(from) })
	hi := sort.Search(len(sessions), func(i int) bool { return !sessions[i].Before(to) })
	if hi < lo {
		return 0
	}
	return hi - lo
}

// archiveHoles splits the unsettled dates inside the archive span into holes
// and pending dates.
//
// A hole is a date the archive should have settled and did not: it is
// unlogged (or logged only as 'error') and the last fetch attempt for this
// source happened AFTER the date's own settlement horizon, so a run had the
// chance to settle it and did not. That is the state G4a refuses to run in,
// because a session that was never fetched is indistinguishable from a
// holiday once G4 counts it out of bars.
//
// A pending date is one the last run could not have settled yet.
// backfill.noFileSettled deliberately leaves a recent 404 unlogged rather
// than writing a terminal 'no-file' for a session NSE has not published yet,
// so the tail of the archive always carries a day or two of unlogged dates
// and they are not evidence of anything. Design 5.3 records the live store in
// exactly this state -- one unlogged date, a Sunday held back for retry --
// and says propose should run clean; treating that tail as a hole would make
// the roster ungeneratable for a reason that is a clock rather than a gap.
// Pending dates are still NOT settled for a candidate's own G4a, so a
// boundary that falls in the tail is quarantined either way.
func archiveHoles(from, to time.Time, settled map[time.Time]bool, lastFetch time.Time) (holes, pending []time.Time) {
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if settled[d] {
			continue
		}
		// midnight IST on d+1 is the end of the session; a further full day
		// of slack absorbs a late publish. See bhavcopy.noFileSettled.
		horizon := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, ist).AddDate(0, 0, 1).Add(settleLag)
		if lastFetch.Before(horizon) {
			pending = append(pending, d)
			continue
		}
		holes = append(holes, d)
	}
	return holes, pending
}

func formatDates(dates []time.Time, max int) string {
	var out []string
	for i, d := range dates {
		if i == max {
			out = append(out, fmt.Sprintf("... and %d more", len(dates)-max))
			break
		}
		out = append(out, d.Format("2006-01-02"))
	}
	s := ""
	for i, d := range out {
		if i > 0 {
			s += ", "
		}
		s += d
	}
	return s
}

func day(t time.Time) string { return t.Format("2006-01-02") }

func round4(f float64) float64 {
	return float64(int64(f*1e4+0.5)) / 1e4
}

func ratio(a, b *float64) *float64 {
	if a == nil || b == nil || *b == 0 {
		return nil
	}
	r := round4(*a / *b)
	return &r
}

func intRatio(a, b *int64) *float64 {
	if a == nil || b == nil || *b == 0 {
		return nil
	}
	r := round4(float64(*a) / float64(*b))
	return &r
}

// WriteReview writes the boundary-continuity report: one JSON object per
// candidate, accepted or not.
//
// It exists because 449 uniform structural rows are not something a reviewer
// can disagree with. The close, turnover and delivery ratios across the
// boundary are non-gating, and they are the only material in the whole
// process that lets a human say "that one looks wrong" about a line the gates
// were happy with.
func WriteReview(w io.Writer, candidates []Candidate) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for i := range candidates {
		if err := enc.Encode(candidates[i]); err != nil {
			return err
		}
	}
	return nil
}

func loadSymbolFacts(ctx context.Context, pool *pgxpool.Pool, asOfIngest time.Time) (map[int64]*symbolFacts, error) {
	// The label laterals mirror market.symbolLabelLateral exactly. They are
	// used for GROUPING and for the evidence block only -- never to decide a
	// read's answer -- but they must agree with the reader or the roster
	// would describe a ticker nobody sees.
	const label = `(
		SELECT ticker FROM symbols
		WHERE symbol_id = e.symbol_id AND ingested_at <= $2
		ORDER BY (valid_from <= %s) DESC,
		         CASE WHEN valid_from <= %s THEN valid_from END DESC,
		         valid_from,
		         ingested_at DESC
		LIMIT 1)`
	q := fmt.Sprintf(`
		WITH latest AS (
			SELECT DISTINCT ON (symbol_id, date) symbol_id, date, close, turnover, delivery_qty
			FROM bars
			WHERE source = $1 AND ingested_at <= $2
			ORDER BY symbol_id, date, ingested_at DESC
		), e AS (
			SELECT symbol_id, min(date) AS first_date, max(date) AS last_date, count(*)::int AS bars
			FROM latest GROUP BY symbol_id
		)
		SELECT e.symbol_id, sy.isin, e.first_date, e.last_date, e.bars,
		       f.close::float8, f.turnover::float8, f.delivery_qty,
		       l.close::float8, l.turnover::float8, l.delivery_qty,
		       %s, %s
		FROM e
		JOIN (SELECT DISTINCT symbol_id, isin FROM symbols WHERE ingested_at <= $2) sy ON sy.symbol_id = e.symbol_id
		JOIN latest f ON f.symbol_id = e.symbol_id AND f.date = e.first_date
		JOIN latest l ON l.symbol_id = e.symbol_id AND l.date = e.last_date`,
		fmt.Sprintf(label, "e.first_date", "e.first_date"),
		fmt.Sprintf(label, "e.last_date", "e.last_date"))

	rows, err := pool.Query(ctx, q, market.SourceBhavcopy, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("propose: symbol facts: %w", err)
	}
	defer rows.Close()
	out := map[int64]*symbolFacts{}
	for rows.Next() {
		f := &symbolFacts{}
		if err := rows.Scan(&f.SymbolID, &f.ISIN, &f.FirstDate, &f.LastDate, &f.Bars,
			&f.FirstClose, &f.FirstTurn, &f.FirstDeliv,
			&f.LastClose, &f.LastTurn, &f.LastDeliv,
			&f.FirstTicker, &f.LastTicker); err != nil {
			return nil, err
		}
		f.FirstDate = market.Day(f.FirstDate.Year(), f.FirstDate.Month(), f.FirstDate.Day())
		f.LastDate = market.Day(f.LastDate.Year(), f.LastDate.Month(), f.LastDate.Day())
		out[f.SymbolID] = f
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	eod, err := pool.Query(ctx, `
		SELECT symbol_id, count(DISTINCT date)::int, min(date)
		FROM bars WHERE source = $1 AND ingested_at <= $2 GROUP BY symbol_id`,
		market.SourceEod2, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("propose: eod2 evidence: %w", err)
	}
	defer eod.Close()
	for eod.Next() {
		var id int64
		var n int
		var first time.Time
		if err := eod.Scan(&id, &n, &first); err != nil {
			return nil, err
		}
		if f, ok := out[id]; ok {
			d := market.Day(first.Year(), first.Month(), first.Day())
			f.Eod2Bars, f.Eod2First = n, &d
		}
	}
	return out, eod.Err()
}

func loadSessions(ctx context.Context, pool *pgxpool.Pool, asOfIngest time.Time) ([]time.Time, error) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT date FROM bars WHERE source = $1 AND ingested_at <= $2 ORDER BY date`,
		market.SourceBhavcopy, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("propose: sessions: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, market.Day(d.Year(), d.Month(), d.Day()))
	}
	return out, rows.Err()
}

// loadSettlement reads ingest_log. A date is settled when SOME attempt at it
// ended 'ok' or 'no-file'; a date whose only rows are errors is not, which is
// the 2020-07-13 shape the repository's own record carries -- logged 'error'
// twice and only 'ok' an hour later.
func loadSettlement(ctx context.Context, pool *pgxpool.Pool, asOfIngest time.Time) (map[time.Time]bool, time.Time, error) {
	rows, err := pool.Query(ctx, `
		SELECT date, bool_or(status IN ('ok', 'no-file')), max(fetched_at)
		FROM ingest_log WHERE source = $1 AND fetched_at <= $2 GROUP BY date`,
		market.SourceBhavcopy, asOfIngest)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("propose: ingest log: %w", err)
	}
	defer rows.Close()
	settled := map[time.Time]bool{}
	var lastFetch time.Time
	for rows.Next() {
		var d time.Time
		var ok bool
		var fetched time.Time
		if err := rows.Scan(&d, &ok, &fetched); err != nil {
			return nil, time.Time{}, err
		}
		settled[market.Day(d.Year(), d.Month(), d.Day())] = ok
		if fetched.After(lastFetch) {
			lastFetch = fetched
		}
	}
	return settled, lastFetch, rows.Err()
}

// measureOverlaps counts the sessions each candidate pair actually SHARES,
// rather than inferring it from the span endpoints. G3's real claim is "no
// date is held by both", and strict span ordering only implies it; a
// candidate whose spans interleave is exactly the catastrophic case and is
// worth measuring rather than deducing.
func measureOverlaps(ctx context.Context, pool *pgxpool.Pool, asOfIngest time.Time, candidates []*Candidate) error {
	if len(candidates) == 0 {
		return nil
	}
	a := make([]int64, len(candidates))
	b := make([]int64, len(candidates))
	for i, c := range candidates {
		a[i], b[i] = c.PredecessorID, c.SuccessorID
	}
	rows, err := pool.Query(ctx, `
		SELECT p.a, p.b, (
			SELECT count(*) FROM (
				SELECT date FROM bars WHERE symbol_id = p.a AND source = $3 AND ingested_at <= $4
				INTERSECT
				SELECT date FROM bars WHERE symbol_id = p.b AND source = $3 AND ingested_at <= $4
			) shared
		)::int
		FROM unnest($1::bigint[], $2::bigint[]) AS p(a, b)`,
		a, b, market.SourceBhavcopy, asOfIngest)
	if err != nil {
		return fmt.Errorf("propose: overlap: %w", err)
	}
	defer rows.Close()
	overlaps := map[[2]int64]int{}
	for rows.Next() {
		var x, y int64
		var n int
		if err := rows.Scan(&x, &y, &n); err != nil {
			return err
		}
		overlaps[[2]int64{x, y}] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range candidates {
		c.OverlapDates = overlaps[[2]int64{c.PredecessorID, c.SuccessorID}]
	}
	return nil
}
