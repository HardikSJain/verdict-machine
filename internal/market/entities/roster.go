package entities

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"
)

// The roster: the file in git that decides identity.
//
// Design 5.1. Nothing is linked automatically after an ingest. The flow is
// `propose` (read-only, machine) -> a reviewed PR containing the roster ->
// `apply` (writes rows, stamping the roster's sha256 into every one). The
// file is read from disk by `apply` only; it is deliberately NOT embedded and
// no read path ever consults it, so it does not become a second source of
// truth for reference data at runtime. What it buys is this project's own
// thesis applied to identity: the map is reproducible from a git sha, and an
// irreversible merge in an insert-only store points back at a diff a human
// approved.

// Roster is the reviewed map from predecessor ISIN to successor ISIN.
//
// Digest is the file's own statement of its sha256 over the canonical form of
// (version, links) -- the same digest `apply` stamps into every row it
// writes. Load recomputes it and refuses a file that disagrees with itself,
// which is what catches a line edited after the review the git diff recorded.
type Roster struct {
	Version int    `json:"version"`
	Digest  string `json:"digest,omitempty"`
	Links   []Link `json:"links"`
}

// Link is one succession: one company's move from one ISIN to the next.
//
// Reason maps to symbol_links.reason. A "succession" line is machine-proposed
// and must satisfy every gate. A "manual" line is hand-written for a
// quarantined pair -- an INF fund-unit transfer, a long-gap relisting -- and
// is exempt from most of the structural gates by construction, since it
// exists precisely because none of them admitted it. It is NOT exempt from
// G0's check digit: that gate exists for hand-written lines specifically, and
// it passes on every INF unit in this archive. In exchange it must name a
// ratifier and carry a note, because design 5.6 is explicit that no
// quarantined pair may be promoted without an external NSE corporate-action
// source, and the schema cannot supply that fact: a human with a circular
// must.
type Link struct {
	Reason            string            `json:"reason"`
	Predecessor       string            `json:"predecessor"`
	Successor         string            `json:"successor"`
	TickerAtBoundary  string            `json:"ticker_at_boundary"`
	EffectiveFrom     string            `json:"effective_from"`
	Gates             Gates             `json:"gates"`
	EvidenceNotGating EvidenceNotGating `json:"evidence_not_gating"`
	RatifiedBy        string            `json:"ratified_by"`
	RatifiedAt        string            `json:"ratified_at"`
	Note              string            `json:"note"`
}

// Gates records every gate result the seeder saw, so the reviewer reads the
// same numbers the validator reads and `apply` can re-run the gates without
// the store.
type Gates struct {
	CheckDigit           bool    `json:"check_digit"`
	IssuerPrefix         string  `json:"issuer_prefix"`
	Serial               string  `json:"serial"`
	PredecessorLastBar   string  `json:"predecessor_last_bar"`
	SuccessorFirstBar    string  `json:"successor_first_bar"`
	SessionsBetween      int     `json:"sessions_between"`
	CalendarDaysBetween  int     `json:"calendar_days_between"`
	GapDatesAllSettled   bool    `json:"gap_dates_all_settled"`
	ArchiveUnsettled     int     `json:"archive_unsettled_dates"`
	BoundaryCloseRatio   float64 `json:"boundary_close_ratio"`
	BoundaryRatioMatches string  `json:"boundary_ratio_matches"`
	PredecessorBars      int     `json:"predecessor_bars"`
	SuccessorBars        int     `json:"successor_bars"`
}

// EvidenceNotGating records what was looked at and deliberately not gated on.
//
// eod2 gates nothing, and that is a deliberate resolution of a disagreement
// between proposals: eod2.LoadDir files a ticker's whole CSV under today's
// ISIN, so its continuity is ticker-based and for a genuine ticker reuse it
// already contains the splice being guarded against. It would corroborate a
// wrong merge exactly as loudly as a real one, and it derives from the same
// NSE files, so Eod2Independent is false on every line by construction.
type EvidenceNotGating struct {
	Eod2ISIN2HistAgrees     bool `json:"eod2_isin2hist_agrees"`
	Eod2Independent         bool `json:"eod2_independent"`
	Eod2PredecessorBars     int  `json:"eod2_predecessor_bars"`
	BoundaryTickerUnchanged bool `json:"boundary_ticker_unchanged"`
}

// Roster reasons. They are the two non-retraction values of
// symbol_links.reason; a retraction is written by `entities retract` and is
// never carried in a roster.
const (
	ReasonSuccession = "succession"
	ReasonManual     = "manual"
)

// RosterVersion is the only roster schema this binary reads.
const RosterVersion = 1

// maxCalendarDaysBetween is G4b: a hard cap on the calendar gap between the
// predecessor's last bar and the successor's first, belt-and-braces and
// independent of the store's own completeness. It reproduces the measured
// population -- 391 next-day pairs plus 77 weekend pairs -- without asking
// bars whether a session existed, which is exactly the question G4 cannot
// answer safely from an archive assembled by a resumable HTTP backfill.
const maxCalendarDaysBetween = 4

// ratioBandTolerance is G6's half-width: the boundary close ratio must lie
// within +-25% of one of the canonical face-value factors.
const ratioBandTolerance = 0.25

// ratioBands are G6's canonical face-value factors. The distribution of the
// measured boundary ratio is BIMODAL rather than centred on 1 -- 278 of 449
// adjacent candidates land in 0.8-1.25 and 168 land at the face-value factor
// -- because the ex-split session can fall on either side of the ISIN
// boundary. At the TATASTEEL boundary the re-basing happens on 2022-07-28,
// one session BEFORE the ISIN changes on 07-29, so the boundary ratio is
// 107.60/100.35 = 1.07; where the re-basing falls on the successor's own
// first session instead, the ratio is the factor itself. A "near 1" rule
// would quarantine 168 genuine successions.
var ratioBands = []struct {
	name  string
	value float64
}{
	{"1/1", 1},
	{"1/2", 0.5},
	{"2/5", 0.4},
	{"1/4", 0.25},
	{"1/5", 0.2},
	{"1/10", 0.1},
	{"1/20", 0.05},
	{"1/50", 0.02},
	{"1/100", 0.01},
}

// MatchRatioBand returns the canonical band the boundary close ratio falls
// in, or "" for a ratio outside every band. Bands overlap (0.4 +-25% and 0.5
// +-25% share [0.375, 0.5]), so the nearest in relative terms wins and the
// answer is deterministic.
func MatchRatioBand(ratio float64) string {
	best, bestErr := "", math.Inf(1)
	for _, b := range ratioBands {
		if ratio < b.value*(1-ratioBandTolerance) || ratio > b.value*(1+ratioBandTolerance) {
			continue
		}
		if e := math.Abs(ratio-b.value) / b.value; e < bestErr {
			best, bestErr = b.name, e
		}
	}
	return best
}

// rosterPayload is the part of a roster the digest is taken over: everything
// except the file's own statement of that digest.
type rosterPayload struct {
	Version int    `json:"version"`
	Links   []Link `json:"links"`
}

// ComputeDigest returns the sha256 over the roster's canonical form, with the
// links sorted by (predecessor, successor) so the digest is order-independent
// and a re-run of `propose` that emits the same map produces the same hash.
func (r *Roster) ComputeDigest() ([]byte, error) {
	links := make([]Link, len(r.Links))
	copy(links, r.Links)
	sortLinks(links)
	return DigestOf(rosterPayload{Version: r.Version, Links: links})
}

func sortLinks(links []Link) {
	sort.SliceStable(links, func(i, j int) bool {
		if links[i].Predecessor != links[j].Predecessor {
			return links[i].Predecessor < links[j].Predecessor
		}
		return links[i].Successor < links[j].Successor
	})
}

// Sort orders the roster's links the way the digest sees them, which is also
// the order that makes the diff readable.
func (r *Roster) Sort() { sortLinks(r.Links) }

// Marshal renders the roster as the file on disk: sorted, indented, with its
// own digest stated.
func (r *Roster) Marshal() ([]byte, error) {
	r.Sort()
	d, err := r.ComputeDigest()
	if err != nil {
		return nil, err
	}
	r.Digest = hex.EncodeToString(d)
	// HTML escaping is off: the file is read by people, and a serial written
	// as "01-\u003e02" is noise in a diff. The digest is unaffected either
	// way, because it is taken over the canonical form rather than over these
	// bytes.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Validate runs the gates over the roster's own recorded facts: G0-G6 on
// every succession line, the ratification requirements on every manual line,
// and G5's graph shape over the set.
//
// It is a filter on the SEEDER, not a proof about NSE. A line claiming
// calendar_days_between = 1 when the archive says 40 passes here; only
// re-running `propose` against the store catches that. What it does catch is
// the class design 5.5 says the reviewer cannot: a hand-edited line, a typo
// in an ISIN, a link that runs backwards, a graph that is not a path.
func (r *Roster) Validate() error {
	if r.Version != RosterVersion {
		return fmt.Errorf("roster: version %d, want %d", r.Version, RosterVersion)
	}
	for _, l := range r.Links {
		if err := validateLink(l); err != nil {
			return fmt.Errorf("roster: %s -> %s: %w", l.Predecessor, l.Successor, err)
		}
	}
	return validateGraph(r.Links)
}

func reasonOf(l Link) string {
	if l.Reason == "" {
		return ReasonSuccession
	}
	return l.Reason
}

func validateLink(l Link) error {
	switch reasonOf(l) {
	case ReasonSuccession, ReasonManual:
	default:
		return fmt.Errorf("reason %q: a roster line is %q or %q; a retraction is written by `entities retract` and is never carried in a roster",
			l.Reason, ReasonSuccession, ReasonManual)
	}
	if l.Predecessor == l.Successor {
		return fmt.Errorf("predecessor and successor are the same ISIN")
	}
	if len(l.Predecessor) != isinLength || len(l.Successor) != isinLength {
		return fmt.Errorf("an ISIN is %d characters", isinLength)
	}
	effective, err := parseDay(l.EffectiveFrom)
	if err != nil {
		return fmt.Errorf("effective_from: %w", err)
	}
	predLast, err := parseDay(l.Gates.PredecessorLastBar)
	if err != nil {
		return fmt.Errorf("gates.predecessor_last_bar: %w", err)
	}
	succFirst, err := parseDay(l.Gates.SuccessorFirstBar)
	if err != nil {
		return fmt.Errorf("gates.successor_first_bar: %w", err)
	}
	if !effective.Equal(succFirst) {
		return fmt.Errorf("effective_from %s is not the successor's first bar %s; the boundary written to the store is the successor's first nse-bhavcopy session",
			l.EffectiveFrom, l.Gates.SuccessorFirstBar)
	}
	// G3, for every kind of line: two members of one entity may never hold a
	// bar on the same session, and the read path errors rather than
	// collapsing them if they do.
	if !predLast.Before(succFirst) {
		return fmt.Errorf("G3: spans are not disjoint: the predecessor's last bar %s is not before the successor's first bar %s",
			l.Gates.PredecessorLastBar, l.Gates.SuccessorFirstBar)
	}

	// G0's check digit, and it runs for EVERY line, manual ones included.
	// What a manual line is exempt from is G0's INE-only half, which is
	// genuinely meaningless for the 51 INF fund-unit pairs a hand-written
	// line exists for. The ISO 6166 mod-10 is not meaningless for them, and
	// design 5.3 justifies it by pointing AT the hand-written line: it
	// "catches a typo in a hand-written roster line, which is exactly where a
	// false positive would originate". It is also free -- across all 1,250
	// ISINs in the 625 candidates, 104 of them non-INE INF/IN9 units and
	// including NIFTYBEES' own INF732E01011 and INF204KB14I2, zero fail it.
	// The residual defence, `apply` refusing an ISIN this store never
	// registered, only fires when the typo lands on nothing: a typo that
	// lands on another REAL company's ISIN is the false positive named, and
	// it goes into a table with no DELETE.
	for _, isin := range []string{l.Predecessor, l.Successor} {
		if !WellFormedISIN(isin) {
			return fmt.Errorf("G0: %s fails the ISO 6166 check digit", isin)
		}
	}
	if !l.Gates.CheckDigit {
		return fmt.Errorf("G0: gates.check_digit is false")
	}

	if reasonOf(l) == ReasonManual {
		// A manual line is exempt from the REST of the gates -- it exists
		// because none of them admitted it -- and in exchange it must point
		// at a person and a reason.
		if l.RatifiedBy == "" {
			return fmt.Errorf("a manual line must name a ratifier in ratified_by: it is the highest-risk row the store can hold, and no gate stands behind it")
		}
		if _, err := parseDay(l.RatifiedAt); err != nil {
			return fmt.Errorf("ratified_at: %w", err)
		}
		if l.Note == "" {
			return fmt.Errorf("a manual line must carry a note saying what external evidence settled it; design 5.6 requires an NSE corporate-action source and the schema cannot supply it")
		}
		return nil
	}

	// G0's other half, which a manual line IS exempt from: INE equity only.
	if !IsEquityISIN(l.Predecessor) || !IsEquityISIN(l.Successor) {
		return fmt.Errorf("G0: an auto-accepted line must join two INE equity ISINs; fund units (INF) go to the quarantine queue, because one INF issuer code covers dozens of unrelated schemes and G1 is meaningless for them")
	}
	// G1. Same NSDL issuer.
	prefix := IssuerPrefix(l.Predecessor)
	if prefix != IssuerPrefix(l.Successor) {
		return fmt.Errorf("G1: issuer prefix %s does not match %s; NSDL assigns the issuer code to the legal entity, so this is two companies",
			prefix, IssuerPrefix(l.Successor))
	}
	if l.Gates.IssuerPrefix != prefix {
		return fmt.Errorf("G1: gates.issuer_prefix %q does not describe these ISINs (%s)", l.Gates.IssuerPrefix, prefix)
	}
	// G2. Serial strictly increasing, which reads the link's direction off
	// the identifier rather than off the dates.
	predSerial, ok := IssueSerial(l.Predecessor)
	if !ok {
		return fmt.Errorf("G2: %s has no readable issue serial", l.Predecessor)
	}
	succSerial, ok := IssueSerial(l.Successor)
	if !ok {
		return fmt.Errorf("G2: %s has no readable issue serial", l.Successor)
	}
	if succSerial <= predSerial {
		return fmt.Errorf("G2: issue serial %02d -> %02d does not increase; the dates and the identifier disagree about which way this link runs",
			predSerial, succSerial)
	}
	if want := fmt.Sprintf("%02d->%02d", predSerial, succSerial); l.Gates.Serial != want {
		return fmt.Errorf("G2: gates.serial %q does not describe these ISINs (%s)", l.Gates.Serial, want)
	}
	// G4. No live session in the gap. This is the gate that kills demergers.
	if l.Gates.SessionsBetween != 0 {
		return fmt.Errorf("G4: %d nse-bhavcopy sessions fall strictly between the two spans; a gap of live sessions is a relisting or a demerger, not a succession",
			l.Gates.SessionsBetween)
	}
	// G4a. The archive the session count was read from has to be complete,
	// because "zero sessions between" and "zero sessions fetched" look
	// identical from inside bars.
	if !l.Gates.GapDatesAllSettled {
		return fmt.Errorf("G4a: not every calendar date in the gap is settled in ingest_log as 'ok' or 'no-file'")
	}
	if l.Gates.ArchiveUnsettled != 0 {
		return fmt.Errorf("G4a: %d unsettled dates inside the archive span; the session count G4 reads is not trustworthy while the archive has holes",
			l.Gates.ArchiveUnsettled)
	}
	// G4b. The calendar cap, which does not ask bars anything.
	if l.Gates.CalendarDaysBetween < 1 {
		return fmt.Errorf("G4b: calendar days between is %d", l.Gates.CalendarDaysBetween)
	}
	if got := int(succFirst.Sub(predLast).Hours() / 24); got != l.Gates.CalendarDaysBetween {
		return fmt.Errorf("G4b: gates.calendar_days_between %d does not describe these dates (%d)", l.Gates.CalendarDaysBetween, got)
	}
	if l.Gates.CalendarDaysBetween > maxCalendarDaysBetween {
		return fmt.Errorf("G4b: %d calendar days between the spans, cap is %d; a longer gap is a suspension or a relisting",
			l.Gates.CalendarDaysBetween, maxCalendarDaysBetween)
	}
	// G6. The one non-structural gate: evidence, not proof.
	band := MatchRatioBand(l.Gates.BoundaryCloseRatio)
	if band == "" {
		return fmt.Errorf("G6: boundary close ratio %.4f is outside every canonical band; a company reusing a freed ticker produces an arbitrary ratio",
			l.Gates.BoundaryCloseRatio)
	}
	if l.Gates.BoundaryRatioMatches != band {
		return fmt.Errorf("G6: gates.boundary_ratio_matches %q does not describe ratio %.4f (%s)",
			l.Gates.BoundaryRatioMatches, l.Gates.BoundaryCloseRatio, band)
	}
	return nil
}

// validateGraph is G5: the accepted set is a set of PATHS. No ISIN is claimed
// twice as a successor or twice as a predecessor, there are no cycles, and
// each chain's spans are totally ordered and disjoint.
//
// It matters because the store resolves identity in exactly one hop: a chain
// A -> B -> C is two rows both carrying entity_id = A, and a graph that
// branches or loops has no root to name the entity after. The measured graph
// is already this shape -- 429 chains of length 2, 37 of length 3, one of
// length 4 and zero predecessors with two successors -- so this gate refuses
// a hand-written line rather than a measured one.
func validateGraph(links []Link) error {
	bySuccessor := map[string]Link{}
	byPredecessor := map[string]Link{}
	for _, l := range links {
		if prev, ok := bySuccessor[l.Successor]; ok {
			return fmt.Errorf("roster: G5: %s is claimed as the successor of both %s and %s; a successor has one predecessor or the map is not a path",
				l.Successor, prev.Predecessor, l.Predecessor)
		}
		bySuccessor[l.Successor] = l
		if prev, ok := byPredecessor[l.Predecessor]; ok {
			return fmt.Errorf("roster: G5: %s is claimed as the predecessor of both %s and %s; a predecessor with two successors is a demerger, not a succession",
				l.Predecessor, prev.Successor, l.Successor)
		}
		byPredecessor[l.Predecessor] = l
	}
	// Walk forward from every root. Anything not reached by a walk is inside
	// a cycle, because every node has in-degree and out-degree at most one.
	seen := map[string]bool{}
	for _, l := range links {
		if _, hasPred := bySuccessor[l.Predecessor]; hasPred {
			continue // not a root
		}
		cur := l
		for {
			seen[cur.Predecessor+">"+cur.Successor] = true
			next, ok := byPredecessor[cur.Successor]
			if !ok {
				break
			}
			// cur.Successor is a member of the chain twice over: it is the
			// successor of cur and the predecessor of next, so its own span
			// runs from one date to the other and must run forwards.
			start, _ := parseDay(cur.Gates.SuccessorFirstBar)
			end, _ := parseDay(next.Gates.PredecessorLastBar)
			if end.Before(start) {
				return fmt.Errorf("roster: G5: %s starts trading on %s but the next link says its last bar was %s; a chain's spans must be totally ordered and disjoint",
					cur.Successor, cur.Gates.SuccessorFirstBar, next.Gates.PredecessorLastBar)
			}
			cur = next
		}
	}
	for _, l := range links {
		if !seen[l.Predecessor+">"+l.Successor] {
			return fmt.Errorf("roster: G5: %s -> %s is in a cycle; a cycle has no chain root, so there is no entity to name",
				l.Predecessor, l.Successor)
		}
	}
	return nil
}

func parseDay(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("missing date")
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("want YYYY-MM-DD, got %q", s)
	}
	return t, nil
}

// Load reads, validates and digests a roster file. It is the only way a
// roster enters this program: `apply` calls it, and so the gates run against
// the file on disk every single time rows are written, not only when they
// were generated.
//
// Unknown fields are an error. A misspelled key in a hand-written line would
// otherwise be a silently empty field -- and an empty gate field is the
// permissive value for several of these checks.
func Load(path string) (*Roster, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var r Roster
	if err := dec.Decode(&r); err != nil {
		return nil, nil, fmt.Errorf("roster %s: %w", path, err)
	}
	// An omitted reason means "succession"; normalising before the digest is
	// taken makes the two spellings hash identically, so a reviewer who adds
	// the explicit word does not invalidate the file.
	for i := range r.Links {
		r.Links[i].Reason = reasonOf(r.Links[i])
	}
	if err := r.Validate(); err != nil {
		return nil, nil, fmt.Errorf("roster %s: %w", path, err)
	}
	digest, err := r.ComputeDigest()
	if err != nil {
		return nil, nil, err
	}
	if r.Digest != "" {
		stated, err := hex.DecodeString(r.Digest)
		if err != nil {
			return nil, nil, fmt.Errorf("roster %s: digest %q is not hex: %w", path, r.Digest, err)
		}
		if !bytes.Equal(stated, digest) {
			return nil, nil, fmt.Errorf(
				"roster %s: stated digest %s does not match its contents (%s); the file was edited after it was digested, and every row apply writes carries this hash as its provenance",
				path, r.Digest, hex.EncodeToString(digest))
		}
	}
	r.Sort()
	return &r, digest, nil
}
