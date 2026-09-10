// Package preisin assigns identity to bars from NSE's archive before it
// printed ISINs.
//
// Everything in this project keys on ISIN: symbols is unique on it, and every
// entity, roster link and universe read resolves through it. NSE began
// printing the column between 2011-06-01 and 2011-07-04, which is why the
// archive here starts 2011-09-02 -- not a choice, just where identity became
// available. The seventeen years before it carry only a ticker.
//
// A ticker is not an identity. It is a label the exchange reuses, and the
// whole succession apparatus exists because this project already learned that
// the hard way: an engine that matched holdings to prices by ticker made
// renamed positions unsellable, and the fix was to key on entity instead.
// Assigning identity here by ticker alone would reintroduce exactly that bug
// across seventeen years, silently, with no error anywhere.
//
// So the rule is evidence, not inference, and the reason is measured. In the
// archive this project already holds, a ticker that moves from one symbol_id
// to another ALWAYS shows a trading gap first -- all 54 such switches have a
// gap above five sessions. That sounds like a usable signal until you look at
// the other column: 3,136 gaps longer than sixty sessions occur while the
// symbol_id does NOT change, because Indian companies are suspended and resume
// all the time. Splitting identity on gap length would break three thousand
// continuous histories to catch fifty-four real switches.
//
// Gap length therefore cannot decide identity. What it can do is decide
// whether the evidence is DIRECT enough to act on, which is what happens
// below: a ticker still trading across the boundary into the ISIN era is the
// same instrument and takes that era's ISIN; a ticker last seen years earlier
// is quarantined rather than merged into whatever later took its label.
//
// # Most quarantines are not reassigned tickers, they are series migrations
//
// A first run over 2011-05-02..2011-07-03 quarantined 28 of 1,488 tickers, and
// the reason was not identity at all. NSE moves a stock between the EQ segment
// and BE -- trade-for-trade surveillance -- and back. Aplab and Gufic Bio both
// traded EQ on 2011-06-09, BE from 2011-07-04, and EQ again in September. They
// never stopped trading for a day.
//
// This archive keeps only EQ rows, so from its point of view those companies
// vanished for three months. Measured across eras, BE runs 2% to 17% of the EQ
// population (48/1199 in 2008, 86/1380 in mid-2011, 40/1447 in 2015, 306/1731
// in 2023), so this is not a rounding error.
//
// It is also almost certainly part of the earlier gap measurement: some of
// those 3,136 "suspensions" longer than sixty sessions are a stock sitting in
// BE, not a stock halted.
//
// Nothing here works around it, and that is deliberate. A quarantine is safe:
// the ticker gets its own identity and its pre-2011 history simply does not
// join to its post-2011 history, which is a visible, bounded, recoverable
// loss. Inferring continuity from an absence we already know we are
// mis-reading would be the unsafe direction. The real repair is upstream --
// ingest BE rows too, which bars.series already has a column for and the
// universe already filters on -- and it is recorded rather than smuggled in
// here.
package preisin

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// SyntheticPrefix marks an identifier this project invented because NSE never
// published one.
//
// It cannot collide with a real ISIN and cannot be mistaken for one: ISO 6166
// requires two letters of ISO country code up front, so no ISIN begins "X-".
// That matters more than tidiness. A fabricated identifier that LOOKED like an
// ISIN would spread through symbols, symbol_links and every join in the
// project with nothing marking it as inferred, and the first person to ask
// "where did this company come from" would have no way to tell.
const SyntheticPrefix = "X-NSE-"

// Synthetic is the identifier for a ticker that has no ISIN anywhere.
func Synthetic(ticker string) string {
	return SyntheticPrefix + strings.ToUpper(strings.TrimSpace(ticker))
}

// IsSynthetic reports whether an identifier was invented here.
func IsSynthetic(isin string) bool { return strings.HasPrefix(isin, SyntheticPrefix) }

// MaxBridgeGapSessions is how far apart a ticker's last pre-ISIN session and
// its first ISIN-era session may be before the bridge stops being direct
// evidence and becomes a guess.
//
// Five, because that is the smallest gap at which the live archive has ever
// seen a ticker change symbol_id -- all 54 observed switches exceed it. Below
// five the label demonstrably stayed with one instrument; above it, this
// project has no way to tell a suspension from a reassignment, and the honest
// answer is to say so rather than pick.
//
// Deliberately strict. A ticker wrongly quarantined costs one company's
// pre-2011 history, which is visible and recoverable. A ticker wrongly bridged
// welds two different companies into one price series, which is invisible and
// poisons every backtest that touches it.
const MaxBridgeGapSessions = 5

// Status is what the resolver concluded, and every one of them is reported.
type Status string

const (
	// Bridged: the ticker was still trading across the boundary, so it takes
	// the ISIN NSE printed for it in the first session where one exists.
	Bridged Status = "bridged"
	// Dead: the ticker never appears in the ISIN era at all AND no later
	// ticker looks like the same company under a new name. A company that
	// stopped trading before July 2011 -- the population that makes this
	// archive worth having, since every adjusted-price source drops them --
	// and it gets a synthetic identifier.
	Dead Status = "dead"
	// Renamed: the ticker vanished at the boundary and another appeared in its
	// place at a matching price, so it is the same company under a new label
	// and takes that company's real ISIN.
	//
	// This case exists because leaving it out is not a small error. Infosys
	// Technologies became Infosys Limited and its ticker went INFOSYSTCH ->
	// INFY in mid-2011: INFOSYSTCH closed 2865.30 on its last session and INFY
	// appears at the boundary at 2938.95 under INE009A01021, a 2.6% drift.
	// Without this, one of the largest companies on the exchange is recorded as
	// having died in June 2011 and its pre-2011 history is severed from the
	// rest -- which is the same failure that once made renamed holdings
	// unsellable in the engine, arriving by a different door.
	Renamed Status = "renamed"
	// Quarantined: the ticker appears in both eras but with a gap too wide to
	// call. Treated as a separate instrument and flagged for review, because
	// the alternative is a silent merge.
	Quarantined Status = "quarantined"
)

// Observation is everything known about one ticker across the two eras.
type Observation struct {
	Ticker string
	// LastPreISIN is its final session in the archive that has no ISIN column,
	// and LastClose the price on it.
	LastPreISIN time.Time
	LastClose   float64
	// FirstISINEra and ISINEraISIN are its first session in the era that does,
	// and the ISIN printed there. Zero and empty when it never reappears.
	FirstISINEra time.Time
	ISINEraISIN  string
}

// Newcomer is a ticker that appears in the ISIN era with no history before it.
// One of these is what a renamed company looks like from the far side.
type Newcomer struct {
	Ticker string
	ISIN   string
	First  time.Time
	Close  float64
}

// RenameTolerance is how far a newcomer's first close may sit from the
// vanished ticker's last close and still be called the same company.
//
// Fifteen percent. A rename is an administrative event, not an economic one --
// the shares do not change, so the price should only drift by whatever the
// market did across the gap. Infosys drifted 2.6% over four sessions. The
// allowance is wide enough for a volatile small cap across a couple of weeks
// and far too narrow to pair two unrelated companies except by coincidence,
// which is what AmbiguousRename exists to catch.
const RenameTolerance = 0.15

// MatchRenames pairs each vanished ticker with the newcomer that replaced it.
//
// Two independent conditions have to hold, which is the same shape as the
// corporate-action detector: a candidate must match on TIMING and be
// corroborated by PRICE. Timing alone would pair any two tickers that happened
// to change around the boundary, and price alone would pair every stock
// trading near the same rupee value.
//
// A vanished ticker matching more than one newcomer is left unpaired. Guessing
// between two candidates is exactly the decision that produces a welded price
// series, and there is no way to notice afterwards.
func MatchRenames(vanished []Observation, newcomers []Newcomer,
	gapSessions func(from, to time.Time) int, maxGap int, tol float64) map[string]Newcomer {
	if tol <= 0 {
		tol = RenameTolerance
	}
	out := map[string]Newcomer{}
	claimed := map[string]int{}
	cand := map[string][]Newcomer{}
	for _, v := range vanished {
		if v.LastClose <= 0 {
			continue
		}
		for _, n := range newcomers {
			if n.Close <= 0 || n.First.Before(v.LastPreISIN) {
				continue
			}
			if g := gapSessions(v.LastPreISIN, n.First); g < 0 || g > maxGap {
				continue
			}
			if math.Abs(n.Close/v.LastClose-1) > tol {
				continue
			}
			cand[v.Ticker] = append(cand[v.Ticker], n)
			claimed[n.Ticker]++
		}
	}
	for ticker, cs := range cand {
		if len(cs) != 1 || claimed[cs[0].Ticker] != 1 {
			// Ambiguous in either direction: two names for one company, or two
			// companies for one name. Not resolvable from prices and dates.
			continue
		}
		out[ticker] = cs[0]
	}
	return out
}

// Assignment is one identity decision, carrying the evidence for it.
type Assignment struct {
	Ticker      string    `json:"ticker"`
	ISIN        string    `json:"isin"`
	Status      Status    `json:"status"`
	Synthetic   bool      `json:"synthetic"`
	LastPreISIN time.Time `json:"last_pre_isin"`
	FirstISIN   time.Time `json:"first_isin_era,omitempty"`
	GapSessions int       `json:"gap_sessions"`
	Note        string    `json:"note"`
}

// Summary counts the decisions so a run reports its own shape.
type Summary struct {
	Bridged, Dead, Quarantined, Renamed int
}

func (s Summary) Total() int { return s.Bridged + s.Dead + s.Quarantined + s.Renamed }

// Resolve decides an identity for every observed ticker.
//
// gapSessions counts trading sessions between two dates in the archive's own
// calendar -- not calendar days, for the same reason the month-end rule is read
// off the archive: holidays and the Saturdays NSE trades make weekday
// arithmetic wrong, and here it would be wrong in the direction of bridging
// things it should not.
func Resolve(obs []Observation, gapSessions func(from, to time.Time) int, maxGap int) ([]Assignment, Summary) {
	return ResolveWithRenames(obs, nil, gapSessions, maxGap)
}

// ResolveWithRenames is Resolve with the rename pairs MatchRenames found.
func ResolveWithRenames(obs []Observation, renames map[string]Newcomer,
	gapSessions func(from, to time.Time) int, maxGap int) ([]Assignment, Summary) {
	if maxGap <= 0 {
		maxGap = MaxBridgeGapSessions
	}
	out := make([]Assignment, 0, len(obs))
	var sum Summary
	for _, o := range obs {
		a := Assignment{
			Ticker:      o.Ticker,
			LastPreISIN: o.LastPreISIN,
			FirstISIN:   o.FirstISINEra,
			GapSessions: -1,
		}
		switch {
		case o.ISINEraISIN == "":
			if n, ok := renames[o.Ticker]; ok {
				a.ISIN, a.Synthetic, a.Status = n.ISIN, false, Renamed
				a.FirstISIN = n.First
				a.GapSessions = gapSessions(o.LastPreISIN, n.First)
				a.Note = fmt.Sprintf("became %s at %.2f against a last close of %.2f", n.Ticker, n.Close, o.LastClose)
				sum.Renamed++
				break
			}
			a.ISIN, a.Synthetic, a.Status = Synthetic(o.Ticker), true, Dead
			a.Note = "never traded once NSE printed ISINs; delisted before July 2011"
			sum.Dead++

		default:
			gap := gapSessions(o.LastPreISIN, o.FirstISINEra)
			a.GapSessions = gap
			if gap <= maxGap {
				a.ISIN, a.Synthetic, a.Status = o.ISINEraISIN, false, Bridged
				a.Note = fmt.Sprintf("traded across the boundary with a %d-session gap", gap)
				sum.Bridged++
				break
			}
			a.ISIN, a.Synthetic, a.Status = Synthetic(o.Ticker), true, Quarantined
			a.Note = fmt.Sprintf(
				"gap of %d sessions before %s reappeared as %s; too wide to tell a suspension from a reassigned ticker, so NOT merged",
				gap, o.Ticker, o.ISINEraISIN)
			sum.Quarantined++
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ticker < out[j].Ticker })
	return out, sum
}

// SessionGapFunc builds the gap counter Resolve needs from an ordered list of
// the archive's own sessions.
//
// Sessions, never calendar days. NSE holds live sessions on some Saturdays and
// closes on holidays no weekday rule predicts, so day arithmetic is wrong --
// and here it is wrong in the dangerous direction, since a stretch of holidays
// would shrink an apparent gap and bridge two instruments that should not be.
// A date the archive does not contain returns a gap wide enough to refuse,
// because "I have never heard of this session" must never read as "adjacent".
func SessionGapFunc(sessions []time.Time) func(from, to time.Time) int {
	idx := make(map[time.Time]int, len(sessions))
	for i, d := range sessions {
		idx[time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)] = i
	}
	norm := func(d time.Time) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	}
	const unknown = 1 << 30
	return func(from, to time.Time) int {
		i, ok := idx[norm(from)]
		if !ok {
			return unknown
		}
		j, ok := idx[norm(to)]
		if !ok {
			return unknown
		}
		if j < i {
			return unknown
		}
		return j - i
	}
}
