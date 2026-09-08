package entities

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// The generator's own arithmetic, with no database.
//
// Design 5.5 is explicit that the reviewer of the roster PR "is checking the
// GENERATOR, not 449 independent facts" and that "a systematic bug in
// `propose` is ratified wholesale". That makes these functions the deliverable
// and `Roster.Validate` no defence at all against them: Validate re-reads the
// numbers `propose` wrote into the file, so a line claiming
// calendar_days_between = 1 when the archive says 40 passes it. Only a test
// that runs the measurement itself can fail when the measurement is wrong.

func dt(y int, m time.Month, d int) time.Time { return market.Day(y, m, d) }

// weekdaysBetween is a stand-in session calendar: NSE's real holidays are not
// what is under test here.
func weekdays(from, to time.Time) []time.Time {
	var out []time.Time
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		out = append(out, d)
	}
	return out
}

func TestCountBetweenIsStrictAtBothEnds(t *testing.T) {
	// G4's whole content is "zero sessions strictly between the two spans",
	// and both endpoints are sessions the two members themselves hold. An
	// off-by-one at either end turns a 1-session demerger gap into an
	// auto-accepted succession, or quarantines every next-day split.
	sessions := []time.Time{
		dt(2017, 8, 21), dt(2017, 8, 22), dt(2017, 8, 23),
		dt(2017, 8, 24), dt(2017, 8, 25), dt(2017, 8, 28),
		dt(2017, 8, 29),
	}
	require.Equal(t, 0, countBetween(sessions, dt(2017, 8, 22), dt(2017, 8, 23)),
		"adjacent sessions have nothing between them")
	require.Equal(t, 0, countBetween(sessions, dt(2017, 8, 25), dt(2017, 8, 28)),
		"a weekend is not a session")
	require.Equal(t, 1, countBetween(sessions, dt(2017, 8, 23), dt(2017, 8, 25)))
	require.Equal(t, 3, countBetween(sessions, dt(2017, 8, 22), dt(2017, 8, 28)))
	require.Equal(t, 0, countBetween(sessions, dt(2017, 8, 29), dt(2017, 9, 5)),
		"a gap past the end of the archive holds no known sessions")
	require.Equal(t, 0, countBetween(nil, dt(2017, 8, 22), dt(2017, 8, 28)))
}

func TestArchiveHolesSeparatesAHoleFromAPendingTail(t *testing.T) {
	// The distinction is the difference between "the archive has a gap" --
	// which G4a must refuse over, because an unfetched session reads as "zero
	// sessions between" -- and "the last run could not have settled this
	// yet", which is backfill.noFileSettled working as designed.
	//
	// A date's horizon is midnight IST after the session plus the settle lag,
	// so with the last fetch at 2017-12-29 10:00Z, 2017-12-27 is long past
	// its chance and 2017-12-28 is not.
	from, to := dt(2017, 12, 20), dt(2017, 12, 29)
	settled := map[time.Time]bool{}
	for _, d := range weekdays(from, to) {
		settled[d] = true
	}
	for _, d := range []time.Time{dt(2017, 12, 23), dt(2017, 12, 24), dt(2017, 12, 30)} {
		settled[d] = true
	}
	lastFetch := time.Date(2017, 12, 29, 10, 0, 0, 0, time.UTC)

	delete(settled, dt(2017, 12, 22)) // a Friday the archive never settled
	delete(settled, dt(2017, 12, 28)) // and the tail the last run could not
	holes, pending := archiveHoles(from, to, settled, lastFetch)
	require.Equal(t, []time.Time{dt(2017, 12, 22)}, holes,
		"a date whose settlement horizon passed before the last fetch is a hole: a run had its chance")
	require.Equal(t, []time.Time{dt(2017, 12, 28)}, pending,
		"and one whose horizon had not passed is pending, which is a clock rather than a gap")

	// The weekend either side of the horizon is the boundary case worth
	// pinning: nothing outside [from, to] is looked at at all.
	holes, pending = archiveHoles(from, to, settled, time.Date(2017, 12, 31, 0, 0, 0, 0, time.UTC))
	require.Len(t, holes, 2, "a later fetch turns the pending tail into a hole")
	require.Empty(t, pending)
}

// factsFor builds one symbol's measured facts the way loadSymbolFacts would.
func factsFor(id int64, isin, ticker string, first, last time.Time, firstClose, lastClose float64) *symbolFacts {
	return &symbolFacts{
		SymbolID: id, ISIN: isin,
		FirstDate: first, LastDate: last, Bars: 100,
		FirstClose: firstClose, LastClose: lastClose,
		FirstTicker: ticker, LastTicker: ticker,
	}
}

func TestGateFailsOnTheRightGateFirst(t *testing.T) {
	// One case per gate, each constructed so that the named gate is the FIRST
	// thing wrong with the pair. The order matters as much as the outcome:
	// the review file reports a candidate by the gate it died on, and a
	// candidate attributed to the wrong gate sends a reviewer to the wrong
	// question.
	sessions := weekdays(dt(2017, 1, 2), dt(2017, 12, 29))
	settled := map[time.Time]bool{}
	for d := dt(2017, 1, 1); !d.After(dt(2017, 12, 31)); d = d.AddDate(0, 0, 1) {
		settled[d] = true
	}

	for _, tc := range []struct {
		name    string
		pred    *symbolFacts
		succ    *symbolFacts
		overlap int
		// closed removes dates from the session calendar, for the one case
		// that needs a gap the exchange itself never traded through.
		closed []time.Time
		want   string // "" means every gate holds
	}{{
		name: "clean adjacent split",
		pred: factsFor(1, "INE081A01012", "TATASTEEL", dt(2017, 1, 2), dt(2017, 7, 28), 959.4, 100.35),
		succ: factsFor(2, "INE081A01020", "TATASTEEL", dt(2017, 7, 31), dt(2017, 12, 29), 107.6, 110),
		want: "",
	}, {
		name: "a fund unit pair is not equity",
		pred: factsFor(1, "INF732E01011", "NIFTYBEES", dt(2017, 1, 2), dt(2017, 7, 28), 100, 100),
		succ: factsFor(2, "INF204KB14I2", "NIFTYBEES", dt(2017, 7, 31), dt(2017, 12, 29), 100, 100),
		want: G0,
	}, {
		name: "two different issuers",
		pred: factsFor(1, "INE081A01012", "TATASTEEL", dt(2017, 1, 2), dt(2017, 7, 28), 100, 100),
		succ: factsFor(2, "INE090A01021", "ICICIBANK", dt(2017, 7, 31), dt(2017, 12, 29), 100, 100),
		want: G1,
	}, {
		name: "the link runs backwards",
		pred: factsFor(1, "INE081A01020", "TATASTEEL", dt(2017, 1, 2), dt(2017, 7, 28), 100, 100),
		succ: factsFor(2, "INE081A01012", "TATASTEEL", dt(2017, 7, 31), dt(2017, 12, 29), 100, 100),
		want: G2,
	}, {
		name:    "the spans share sessions",
		pred:    factsFor(1, "INE081A01012", "TATASTEEL", dt(2017, 1, 2), dt(2017, 7, 28), 100, 100),
		succ:    factsFor(2, "INE081A01020", "TATASTEEL", dt(2017, 7, 31), dt(2017, 12, 29), 100, 100),
		overlap: 4,
		want:    G3,
	}, {
		// The 2017 Tube Investments demerger's shape: disjoint, same issuer,
		// serial increasing, and only the live sessions in the gap stop it.
		name: "live sessions in the gap",
		pred: factsFor(1, "INE149A01025", "TIINDIA", dt(2017, 1, 2), dt(2017, 8, 23), 100, 100),
		succ: factsFor(2, "INE149A01033", "TIINDIA", dt(2017, 10, 10), dt(2017, 12, 29), 100, 100),
		want: G4,
	}, {
		name: "a gap date the archive never settled",
		pred: factsFor(1, "INE081A01012", "TATASTEEL", dt(2017, 1, 2), dt(2017, 8, 4), 100, 100),
		succ: factsFor(2, "INE081A01020", "TATASTEEL", dt(2017, 8, 7), dt(2017, 12, 29), 100, 100),
		want: G4a,
	}, {
		// The live archive holds exactly one of these: INE659A01015 ->
		// INE659A01023, zero sessions in the gap and six calendar days. It is
		// the single disagreement design 5.4 predicted between a
		// session-counting and a calendar-counting generator, and it is why
		// the roster has 444 lines rather than "about 445".
		name:   "a six-day gap with no sessions in it",
		pred:   factsFor(1, "INE081A01012", "TATASTEEL", dt(2017, 1, 2), dt(2017, 7, 28), 100, 100),
		succ:   factsFor(2, "INE081A01020", "TATASTEEL", dt(2017, 8, 3), dt(2017, 12, 29), 100, 100),
		closed: []time.Time{dt(2017, 7, 31), dt(2017, 8, 1), dt(2017, 8, 2)},
		want:   G4b,
	}, {
		name: "a boundary ratio outside every band",
		pred: factsFor(1, "INE081A01012", "TATASTEEL", dt(2017, 1, 2), dt(2017, 7, 28), 100, 100),
		succ: factsFor(2, "INE081A01020", "TATASTEEL", dt(2017, 7, 31), dt(2017, 12, 29), 65, 65),
		want: G6,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			cal := sessions
			if len(tc.closed) > 0 {
				cal = nil
				for _, d := range sessions {
					closed := false
					for _, c := range tc.closed {
						closed = closed || c.Equal(d)
					}
					if !closed {
						cal = append(cal, d)
					}
				}
			}
			s := settled
			if tc.want == G4a {
				// Exactly one date in this pair's own gap is unsettled; the
				// archive-wide count stays zero, so G4a fires for the gap and
				// not for the archive.
				s = map[time.Time]bool{}
				for k, v := range settled {
					s[k] = v
				}
				delete(s, dt(2017, 8, 5))
			}
			c := newCandidate("issuer-prefix", tc.pred, tc.succ)
			c.OverlapDates = tc.overlap
			gate(c, cal, s, 0)
			if tc.want == "" {
				require.Empty(t, c.FailedGates, "this pair must be auto-accepted")
				require.True(t, c.Accepted())
				return
			}
			require.NotEmpty(t, c.FailedGates)
			require.Equal(t, tc.want, c.FailedGates[0],
				"first failure should be %s, got %v", tc.want, c.FailedGates)
		})
	}
}

func TestGateRecordsWhatItMeasured(t *testing.T) {
	// The numbers written into the roster are the numbers Roster.Validate
	// re-reads, so a mis-measured gate produces a file that validates, digests
	// and reviews clean. These are the measurements themselves.
	sessions := weekdays(dt(2017, 1, 2), dt(2017, 12, 29))
	settled := map[time.Time]bool{}
	for d := dt(2017, 1, 1); !d.After(dt(2017, 12, 31)); d = d.AddDate(0, 0, 1) {
		settled[d] = true
	}
	pred := factsFor(1, "INE149A01025", "TIINDIA", dt(2017, 1, 2), dt(2017, 8, 23), 200, 200)
	succ := factsFor(2, "INE149A01033", "TIINDIA", dt(2017, 10, 10), dt(2017, 12, 29), 100, 120)

	c := newCandidate("issuer-prefix", pred, succ)
	gate(c, sessions, settled, 0)
	require.Equal(t, 33, c.Gates.SessionsBetween, "the weekdays strictly inside 2017-08-23..2017-10-10")
	require.Equal(t, 48, c.Gates.CalendarDaysBetween)
	require.True(t, c.Gates.GapDatesAllSettled)
	require.Equal(t, "INE149A01", c.Gates.IssuerPrefix)
	require.Equal(t, "02->03", c.Gates.Serial)
	require.True(t, c.Gates.CheckDigit)
	require.InDelta(t, 0.5, c.Gates.BoundaryCloseRatio, 1e-9, "100 over 200")
	require.Equal(t, "1/2", c.Gates.BoundaryRatioMatches)
	require.Equal(t, "2017-08-23", c.Gates.PredecessorLastBar)
	require.Equal(t, "2017-10-10", c.Gates.SuccessorFirstBar)

	// And the archive-wide refusal is carried onto every line, so a reviewer
	// reading one roster line can see the state the whole run was made in.
	c = newCandidate("issuer-prefix", pred, succ)
	gate(c, sessions, settled, 3)
	require.Equal(t, 3, c.Gates.ArchiveUnsettled)
	require.Contains(t, c.FailedGates, G4a)
}

func TestApplyPathGateQuarantinesBothSidesOfAConflict(t *testing.T) {
	// G5 is a property of the accepted SET, not of a line. Two candidates
	// claiming one successor are both quarantined rather than arbitrated:
	// choosing between two claims is exactly the judgement this design
	// refuses to make by machine, and `apply`'s chain-root walk would resolve
	// it arbitrarily into an irreversible row.
	mk := func(p, s string) *Candidate {
		return &Candidate{Predecessor: p, Successor: s}
	}
	a := mk("INE081A01012", "INE081A01038")
	b := mk("INE081A01020", "INE081A01038")
	clean := mk("INE090A01013", "INE090A01021")
	applyPathGate([]*Candidate{a, b, clean})
	require.Equal(t, []string{G5}, a.FailedGates)
	require.Equal(t, []string{G5}, b.FailedGates)
	require.Empty(t, clean.FailedGates, "an unrelated pair is untouched")

	// The predecessor direction is the demerger shape and is treated the same.
	x := mk("INE081A01012", "INE081A01020")
	y := mk("INE081A01012", "INE081A01038")
	applyPathGate([]*Candidate{x, y})
	require.Equal(t, []string{G5}, x.FailedGates)
	require.Equal(t, []string{G5}, y.FailedGates)

	// A candidate already quarantined by another gate is not part of the
	// accepted set and cannot create a conflict for a line that would
	// otherwise stand.
	dead := mk("INE081A01012", "INE081A01038")
	dead.FailedGates = []string{G4}
	live := mk("INE081A01020", "INE081A01038")
	applyPathGate([]*Candidate{dead, live})
	require.Equal(t, []string{G4}, dead.FailedGates)
	require.Empty(t, live.FailedGates)
}

// pairsOf flattens a generated candidate list into (predecessor, successor)
// pairs in the order the generator emitted them.
func pairsOf(cands []*Candidate) [][2]string {
	out := make([][2]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, [2]string{c.Predecessor, c.Successor})
	}
	return out
}

func TestGenerateChainsConsecutiveMembersOnly(t *testing.T) {
	// The single most consequential property of the generator, and the one
	// `Roster.Validate` is least able to see: a company with three ISINs must
	// produce TWO links, first->second and second->third, and never
	// first->third.
	//
	// A first->third link passes every gate it is asked. G1 holds (same
	// issuer), G2 holds (01 -> 03 increases), G3 holds (the spans are
	// disjoint), G6 holds if the two closes happen to land in a band -- and
	// G4 is the only thing standing between it and acceptance, on the
	// accident that the SECOND ISIN traded in the gap. It would then reach
	// `applyPathGate` as a second claim on the third ISIN, quarantining the
	// GENUINE second->third link along with it: one bad pair silently
	// removing a real company from the map. The whole shape argument -- "the
	// generated graph is a set of paths rather than a mesh" -- rests on this
	// loop pairing g[i] with g[i+1] only.
	facts := map[int64]*symbolFacts{
		1: factsFor(1, "INE777C01011", "POCL", dt(2015, 3, 2), dt(2019, 5, 24), 100, 100),
		2: factsFor(2, "INE777C01029", "POCL", dt(2019, 5, 27), dt(2022, 1, 2), 100, 100),
		3: factsFor(3, "INE777C01037", "POCL", dt(2022, 1, 3), dt(2026, 9, 4), 100, 100),
	}
	got := pairsOf(generate(facts))
	require.Equal(t, [][2]string{
		{"INE777C01011", "INE777C01029"},
		{"INE777C01029", "INE777C01037"},
	}, got, "two consecutive links, and no first-to-third")

	// A four-ISIN chain is three pairs, not six: the live archive holds one
	// such company and 29 chains of three.
	facts[4] = factsFor(4, "INE777C01045", "POCL", dt(2026, 9, 5), dt(2026, 9, 7), 100, 100)
	require.Len(t, generate(facts), 3)

	// Members are ordered by their SPANS, not by the serial and not by the
	// symbol_id: the registry assigns ids in discovery order, and 8.14's
	// fixture registers every successor before its predecessor precisely
	// because that is the live store's shape.
	scrambled := map[int64]*symbolFacts{
		9: factsFor(9, "INE777C01011", "POCL", dt(2015, 3, 2), dt(2019, 5, 24), 100, 100),
		7: factsFor(7, "INE777C01029", "POCL", dt(2019, 5, 27), dt(2022, 1, 2), 100, 100),
		8: factsFor(8, "INE777C01037", "POCL", dt(2022, 1, 3), dt(2026, 9, 4), 100, 100),
	}
	require.Equal(t, got, pairsOf(generate(scrambled)))

	// And a symbol that is alone in its issuer group produces nothing at all,
	// which is the ~3,600 names that never changed ISIN.
	require.Empty(t, generate(map[int64]*symbolFacts{
		1: factsFor(1, "INE666D01014", "CONTROL", dt(2015, 3, 2), dt(2026, 9, 4), 100, 100),
	}))
}

func TestGenerateEmitsEachPairOnceAcrossBothGenerators(t *testing.T) {
	// Both generators run over the same symbols. The ticker pass exists to
	// SURFACE the fund-unit cases, and on an equity chain whose ticker never
	// changed it re-finds the identical pair. Emitting it twice would give
	// the successor two identical claims, and `applyPathGate` -- which counts
	// claims and does not compare them -- would quarantine the pair as a
	// merge point with itself.
	facts := map[int64]*symbolFacts{
		1: factsFor(1, "INE081A01012", "TATASTEEL", dt(2015, 3, 2), dt(2022, 7, 28), 100, 100),
		2: factsFor(2, "INE081A01020", "TATASTEEL", dt(2022, 7, 29), dt(2026, 9, 4), 100, 100),
	}
	cands := generate(facts)
	require.Len(t, cands, 1)
	require.Equal(t, "issuer-prefix", cands[0].Generator,
		"the ISIN-keyed pass runs first, so it is the one recorded")
	applyPathGate(cands)
	require.Empty(t, cands[0].FailedGates, "a pair found twice is one candidate, not a merge point")

	// The ticker pass on its own is what the fund units need: no INE prefix
	// exists to group INF732E01011 with INF204KB14I2, so only the shared
	// ticker pairs them, and they land in the review file as design 5.4 asks.
	units := map[int64]*symbolFacts{
		1: factsFor(1, "INF732E01011", "NIFTYBEES", dt(2015, 3, 2), dt(2019, 12, 19), 100, 100),
		2: factsFor(2, "INF204KB14I2", "NIFTYBEES", dt(2019, 12, 20), dt(2026, 9, 4), 100, 100),
	}
	cands = generate(units)
	require.Len(t, cands, 1)
	require.Equal(t, "ticker", cands[0].Generator)
}

func TestGenerateIsDeterministicAndSoIsTheRosterDigest(t *testing.T) {
	// The roster's sha256 is stamped into every row `apply` writes and is the
	// only thing pointing an irreversible merge back at a reviewed diff. It
	// is taken over the roster's canonical form, so it is stable under
	// re-ordering -- but only if the generator's OUTPUT is stable, and
	// `generate` starts from a map. Go randomises map iteration on every
	// range, so an unsorted group key or an unsorted group would give the
	// same store a different roster, a different digest, and a monthly ops
	// diff that churns for no reason. Once, by luck, is not evidence: this
	// runs the whole thing repeatedly.
	facts := func() map[int64]*symbolFacts {
		return map[int64]*symbolFacts{
			1:  factsFor(1, "INE777C01011", "POCL", dt(2015, 3, 2), dt(2019, 5, 24), 100, 100),
			2:  factsFor(2, "INE777C01029", "POCL", dt(2019, 5, 27), dt(2022, 1, 2), 100, 100),
			3:  factsFor(3, "INE777C01037", "POCL", dt(2022, 1, 3), dt(2026, 9, 4), 100, 100),
			4:  factsFor(4, "INE081A01012", "TATASTEEL", dt(2015, 3, 2), dt(2022, 7, 28), 100, 100),
			5:  factsFor(5, "INE081A01020", "TATASTEEL", dt(2022, 7, 29), dt(2026, 9, 4), 100, 100),
			6:  factsFor(6, "INE090A01013", "ICICIBANK", dt(2015, 3, 2), dt(2019, 5, 24), 100, 100),
			7:  factsFor(7, "INE090A01021", "ICICIBANK", dt(2019, 5, 27), dt(2026, 9, 4), 100, 100),
			8:  factsFor(8, "INF732E01011", "NIFTYBEES", dt(2015, 3, 2), dt(2019, 12, 19), 100, 100),
			9:  factsFor(9, "INF204KB14I2", "NIFTYBEES", dt(2019, 12, 20), dt(2026, 9, 4), 100, 100),
			10: factsFor(10, "INE666D01014", "CONTROL", dt(2015, 3, 2), dt(2026, 9, 4), 100, 100),
		}
	}
	sessions := []time.Time{}
	settled := map[time.Time]bool{}
	for d := dt(2015, 3, 2); !d.After(dt(2026, 9, 7)); d = d.AddDate(0, 0, 1) {
		settled[d] = true
	}

	run := func() ([][2]string, string) {
		cands := generate(facts())
		for _, c := range cands {
			gate(c, sessions, settled, 0)
		}
		applyPathGate(cands)
		r := &Roster{Version: RosterVersion}
		for _, c := range cands {
			if c.Accepted() {
				r.Links = append(r.Links, c.link(ProposeOptions{}))
			}
		}
		b, err := r.Marshal()
		require.NoError(t, err)
		require.NotEmpty(t, r.Digest)
		require.NoError(t, r.Validate())
		return pairsOf(cands), string(b)
	}

	wantPairs, wantFile := run()
	require.NotEmpty(t, wantPairs)
	for i := 0; i < 32; i++ {
		pairs, file := run()
		require.Equal(t, wantPairs, pairs, "the candidate order is a function of the facts, not of map iteration")
		require.Equal(t, wantFile, file, "and so are the roster's bytes, including its stated sha256")
	}
}
