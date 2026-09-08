package entities_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market/entities"
)

// Design 8.12: the roster unit tests. No database.
//
// These are the gates as the roster records them, checked against the
// roster's own recorded facts rather than against the store -- which is the
// whole point of writing the gate results into the file. `apply` re-runs them
// before it writes, so a roster line hand-edited after review to say
// something the seeder never measured is refused at the door, and a reviewer
// reading the diff is reading the same numbers the validator reads.
//
// What they cannot do is check the file against the world: a line claiming
// calendar_days_between = 1 when the archive says 40 passes here and is
// caught only by re-running `entities propose` against the store. The gates
// are a filter on the seeder, not a proof about NSE.

// tataSteel is the design 5.1 example line: the 1:10 face value split of
// 2022-07-29, whose boundary ratio is 1.0723 because the ex-split session
// falls one session BEFORE the ISIN changes.
func tataSteel() entities.Link {
	return entities.Link{
		Reason:           entities.ReasonSuccession,
		Predecessor:      "INE081A01012",
		Successor:        "INE081A01020",
		TickerAtBoundary: "TATASTEEL",
		EffectiveFrom:    "2022-07-29",
		Gates: entities.Gates{
			CheckDigit:           true,
			IssuerPrefix:         "INE081A01",
			Serial:               "01->02",
			PredecessorLastBar:   "2022-07-28",
			SuccessorFirstBar:    "2022-07-29",
			SessionsBetween:      0,
			CalendarDaysBetween:  1,
			GapDatesAllSettled:   true,
			ArchiveUnsettled:     0,
			BoundaryCloseRatio:   1.0723,
			BoundaryRatioMatches: "1/1",
			PredecessorBars:      2703,
			SuccessorBars:        1018,
		},
		EvidenceNotGating: entities.EvidenceNotGating{
			Eod2ISIN2HistAgrees:     true,
			Eod2Independent:         false,
			Eod2PredecessorBars:     0,
			BoundaryTickerUnchanged: true,
		},
		RatifiedBy: "hardik",
		RatifiedAt: "2026-09-08",
		Note:       "1:10 face value split",
	}
}

// plainSplit is the other mode of the bimodal G6 distribution: the re-basing
// falls on the successor's own first session, so the boundary ratio is the
// face-value factor itself.
func plainSplit() entities.Link {
	l := tataSteel()
	l.Predecessor, l.Successor = "INE090A01013", "INE090A01021"
	l.TickerAtBoundary = "ICICIBANK"
	l.Gates.IssuerPrefix = "INE090A01"
	l.Gates.BoundaryCloseRatio = 0.10
	l.Gates.BoundaryRatioMatches = "1/10"
	l.Note = "1:10, re-based on the successor's first session"
	return l
}

func roster(links ...entities.Link) *entities.Roster {
	return &entities.Roster{Version: 1, Links: links}
}

func TestRosterAcceptsBothModesOfTheBoundaryRatio(t *testing.T) {
	// G6's band is deliberately not "near 1": measured across all 449
	// adjacent candidates the distribution is bimodal, 278 near 1 and 168 at
	// the face-value factor, so a near-1 rule would quarantine 168 genuine
	// successions. Both modes must pass.
	require.NoError(t, roster(tataSteel(), plainSplit()).Validate())
}

func TestRosterRejectsABadCheckDigit(t *testing.T) {
	l := tataSteel()
	l.Successor = "INE081A01021" // one digit off the real INE081A01020
	err := roster(l).Validate()
	require.ErrorContains(t, err, "check digit",
		"G0 catches a typo in a hand-written roster line, which is exactly where a false positive would originate")
}

func TestRosterRejectsAnIssuerPrefixMismatch(t *testing.T) {
	l := tataSteel()
	l.Successor = "INE090A01021" // a real ISIN, a different company
	err := roster(l).Validate()
	require.ErrorContains(t, err, "issuer",
		"G1 is the gate that kills cross-company ticker reuse categorically, whatever the dates and prices look like")
}

func TestRosterRejectsASerialThatDoesNotIncrease(t *testing.T) {
	// The link runs backwards: the successor carries the LOWER issue serial.
	l := tataSteel()
	l.Predecessor, l.Successor = "INE081A01020", "INE081A01012"
	l.Gates.Serial = "02->01"
	err := roster(l).Validate()
	require.ErrorContains(t, err, "serial",
		"G2 reads the direction of the link off the identifier; when the dates and the serial disagree, refuse")
}

func TestRosterRejectsAnISINClaimedTwiceAsASuccessor(t *testing.T) {
	// Both links have to be well-formed on their own or this never reaches
	// G5: an earlier version pointed the second link at INE081A01020 from a
	// HIGHER serial, so it died at G2 and the test passed on the wrong error
	// (the G2 message contains the ISIN too, via the `roster: %s -> %s:`
	// wrapper). Disabling G5 outright left it green. Both of these pass
	// G0-G4b and G6 and collide only at the successor.
	a := tataSteel()
	a.Successor = "INE081A01038"
	a.Gates.Serial = "01->03"
	b := tataSteel()
	b.Predecessor = "INE081A01020"
	b.Successor = "INE081A01038"
	b.Gates.Serial = "02->03"
	err := roster(a, b).Validate()
	require.Error(t, err, "G5: the map must be a path, and two predecessors for one successor is a merge point")
	require.Contains(t, err.Error(), "G5", "and it must be G5 that says so, not some other gate")
	require.Contains(t, err.Error(), "INE081A01038")
	require.Contains(t, err.Error(), "INE081A01012")
	require.Contains(t, err.Error(), "INE081A01020")

	// `apply` resolves a chain to its root by walking the roster, so a
	// successor with two predecessors would be resolved arbitrarily into a
	// row that cannot be deleted.
	require.ErrorContains(t, roster(a, b).Validate(), "one predecessor")
}

func TestRosterRejectsABadCheckDigitOnAManualLineToo(t *testing.T) {
	// G0 has two halves and a manual line is exempt from ONE of them. The
	// INE-only half is genuinely meaningless for the 51 INF fund-unit pairs a
	// hand-written line exists for. The ISO 6166 mod-10 is not: design 5.3
	// justifies it by pointing AT the hand-written line -- it "catches a typo
	// in a hand-written roster line, which is exactly where a false positive
	// would originate" -- and it passes on every INF and IN9 unit in this
	// archive, so enforcing it costs nothing and refuses nothing genuine.
	fund := entities.Link{
		Reason:        entities.ReasonManual,
		Predecessor:   "INF732E01011",
		Successor:     "INF204KB14I2",
		EffectiveFrom: "2019-12-20",
		Gates:         entities.Gates{CheckDigit: true, PredecessorLastBar: "2019-12-19", SuccessorFirstBar: "2019-12-20"},
		RatifiedBy:    "hardik", RatifiedAt: "2026-09-08",
		Note: "NSE circular: AMC transfer, Goldman Sachs MF to Nippon India MF",
	}
	require.NoError(t, roster(fund).Validate(), "the exemption that remains: a fund unit pair may be hand-written")

	typo := fund
	typo.Successor = "INE090A01014" // one digit off ICICIBANK's real INE090A01013
	require.ErrorContains(t, roster(typo).Validate(), "check digit",
		"a typo in a hand-written line is the one thing this gate exists for")

	unchecked := fund
	unchecked.Gates.CheckDigit = false
	require.ErrorContains(t, roster(unchecked).Validate(), "check_digit",
		"and a line claiming the digit was never checked is refused as loudly")

	// What it still cannot catch, said out loud rather than implied: a typo
	// that lands on a DIFFERENT real ISIN passes the check digit. Only
	// `apply` refusing an unregistered ISIN stands behind that, and it fires
	// only when the typo lands on nothing.
	real := fund
	real.Successor = "INE090A01013"
	require.NoError(t, roster(real).Validate())
}

func TestRosterRejectsAPredecessorWithTwoSuccessors(t *testing.T) {
	a := tataSteel()
	b := tataSteel()
	b.Successor = "INE081A01038"
	b.Gates.Serial = "01->03"
	err := roster(a, b).Validate()
	require.Error(t, err, "G5: a predecessor with two successors is a demerger, not a succession")
}

func TestRosterRejectsACycle(t *testing.T) {
	// A -> B -> C -> A. Every line is well-formed on its own and every ISIN
	// has in-degree and out-degree exactly one, so only the graph check sees
	// it. A cycle has no root, so `apply` could not name the entity even if
	// it wanted to.
	//
	// The lines are manual, and they have to be: G2 requires the issue serial
	// to increase along every succession line, and a cycle of increasing
	// serials cannot exist. A cycle can only be hand-written, which is
	// exactly the class of line this gate is for.
	mk := func(p, s, last, first string) entities.Link {
		return entities.Link{
			Reason: entities.ReasonManual, Predecessor: p, Successor: s,
			EffectiveFrom: first,
			// check_digit is recorded even on a hand-written line: G0's
			// mod-10 half is not exempted, because it is free on every INF
			// and IN9 unit in this archive and it is the one gate design 5.3
			// justifies by pointing at the hand-written line.
			Gates:      entities.Gates{CheckDigit: true, PredecessorLastBar: last, SuccessorFirstBar: first},
			RatifiedBy: "hardik", RatifiedAt: "2026-09-08", Note: "hand-written",
		}
	}
	err := roster(
		mk("INE777C01011", "INE777C01029", "2015-03-10", "2015-03-11"),
		mk("INE777C01029", "INE777C01037", "2019-06-04", "2019-06-05"),
		mk("INE777C01037", "INE777C01011", "2022-01-06", "2022-01-07"),
	).Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle")
}

// chainLink builds one line of a multi-link chain with its own spans.
func chainLink(p, s, serial, last, first string) entities.Link {
	l := tataSteel()
	l.Predecessor, l.Successor, l.Gates.Serial = p, s, serial
	l.Gates.IssuerPrefix = "INE777C01"
	l.Gates.PredecessorLastBar, l.Gates.SuccessorFirstBar, l.EffectiveFrom = last, first, first
	l.Gates.CalendarDaysBetween = 1
	return l
}

func TestRosterAcceptsAChainOfThree(t *testing.T) {
	r := roster(
		chainLink("INE777C01011", "INE777C01029", "01->02", "2015-03-10", "2015-03-11"),
		chainLink("INE777C01029", "INE777C01037", "02->03", "2019-06-04", "2019-06-05"),
	)
	require.NoError(t, r.Validate(), "a chain is a path, and POCL's four-ISIN chain must be expressible")
}

func TestRosterRejectsAChainWhoseSpansAreNotOrdered(t *testing.T) {
	// B succeeds A in 2019, C succeeds B in 2015: the chain's own dates say
	// the middle member's span contains its successor's.
	r := roster(
		chainLink("INE777C01011", "INE777C01029", "01->02", "2019-06-04", "2019-06-05"),
		chainLink("INE777C01029", "INE777C01037", "02->03", "2015-03-10", "2015-03-11"),
	)
	require.Error(t, r.Validate(), "G5 requires the spans of a chain to be totally ordered and disjoint")
}

func TestRosterRejectsANonDisjointPair(t *testing.T) {
	l := tataSteel()
	l.Gates.PredecessorLastBar = "2022-07-29" // the successor's own first bar
	err := roster(l).Validate()
	require.ErrorContains(t, err, "disjoint",
		"G3: the predecessor's last session must be strictly before the successor's first")
}

func TestRosterRejectsASessionInTheGap(t *testing.T) {
	l := tataSteel()
	l.Gates.SessionsBetween = 31 // the 2017 Tube Investments demerger's gap
	err := roster(l).Validate()
	require.ErrorContains(t, err, "session",
		"G4 is the gate that kills demergers, and 31 live sessions in the gap is exactly the Tube Investments case")
}

func TestRosterRejectsAFiveCalendarDayGap(t *testing.T) {
	// G4b, the belt-and-braces cap. It is independent of the store's own
	// completeness: a gap of real NSE sessions the archive has not fetched
	// yet reads as "zero sessions between", and this cap does not ask bars
	// anything. Four days covers the measured population (391 next-day pairs
	// plus 77 weekend pairs) and five does not.
	l := tataSteel()
	l.Gates.PredecessorLastBar = "2022-07-24"
	l.Gates.CalendarDaysBetween = 5
	require.ErrorContains(t, roster(l).Validate(), "calendar day")

	l.Gates.PredecessorLastBar = "2022-07-25"
	l.Gates.CalendarDaysBetween = 4
	require.NoError(t, roster(l).Validate(), "four days is a long weekend and must pass")
}

func TestRosterRejectsAnUnsettledArchive(t *testing.T) {
	// G4a. G4 counts sessions out of bars, and bars completeness is an
	// operational property of a resumable HTTP backfill, not an invariant: a
	// real session the store has not fetched reads as "zero sessions
	// between" and a demerger sails through every other gate.
	l := tataSteel()
	l.Gates.GapDatesAllSettled = false
	require.ErrorContains(t, roster(l).Validate(), "settled")

	l = tataSteel()
	l.Gates.ArchiveUnsettled = 1
	require.ErrorContains(t, roster(l).Validate(), "unsettled")
}

func TestRosterRejectsABoundaryRatioOutsideEveryBand(t *testing.T) {
	// G6, the only non-structural gate. The bands are +-25% of one of
	// {1, 1/2, 2/5, 1/4, 1/5, 1/10, 1/20, 1/50, 1/100}; 0.65 falls between
	// the 1/2 band (0.375-0.625) and the 1/1 band (0.75-1.25).
	l := tataSteel()
	l.Gates.BoundaryCloseRatio = 0.65
	l.Gates.BoundaryRatioMatches = "1/1"
	require.ErrorContains(t, roster(l).Validate(), "boundary close ratio")

	// And the recorded band has to be the one the ratio actually matches: a
	// line whose ratio is fine but whose band string was hand-edited is a
	// line whose evidence does not describe its own number.
	l = tataSteel()
	l.Gates.BoundaryRatioMatches = "1/10"
	require.Error(t, roster(l).Validate())
}

func TestRosterAcceptsEveryGenuineRatioMode(t *testing.T) {
	for _, tc := range []struct {
		ratio float64
		band  string
	}{{1.0723, "1/1"}, {0.8901, "1/1"}, {1.2, "1/1"}, {0.10, "1/10"}, {0.2, "1/5"}, {0.5844, "1/2"}, {0.02, "1/50"}} {
		l := tataSteel()
		l.Gates.BoundaryCloseRatio = tc.ratio
		l.Gates.BoundaryRatioMatches = tc.band
		require.NoErrorf(t, roster(l).Validate(), "ratio %v band %s must pass", tc.ratio, tc.band)
	}
}

func TestRosterRejectsAFundUnitPairAsAnAutoAcceptedLine(t *testing.T) {
	// The 51 INF pairs have no principled answer -- one INF issuer code
	// covers dozens of unrelated schemes -- and they are quarantined rather
	// than linked. Only a hand-written manual line can carry one.
	l := tataSteel()
	l.Predecessor, l.Successor = "INF732E01011", "INF204KB14I2"
	require.Error(t, roster(l).Validate())
}

func TestRosterManualLineRequiresANoteAndARatifier(t *testing.T) {
	// A manual line is the highest-risk row the store can hold: it is
	// exactly the one no gate admitted. It is exempt from the structural
	// gates (that is what makes it manual) and in exchange it must say who
	// decided and why, so the irreversible row points at a person.
	l := entities.Link{
		Reason:        entities.ReasonManual,
		Predecessor:   "INF732E01011",
		Successor:     "INF204KB14I2",
		EffectiveFrom: "2019-12-20",
		Gates:         entities.Gates{CheckDigit: true, PredecessorLastBar: "2019-12-19", SuccessorFirstBar: "2019-12-20"},
	}
	require.ErrorContains(t, roster(l).Validate(), "ratified_by")

	l.RatifiedBy, l.RatifiedAt = "hardik", "2026-09-08"
	require.ErrorContains(t, roster(l).Validate(), "note")

	l.Note = "NSE circular: AMC transfer, Goldman Sachs MF to Nippon India MF"
	require.NoError(t, roster(l).Validate())
}

func TestRosterRejectsAnUnknownReason(t *testing.T) {
	l := tataSteel()
	l.Reason = "retraction"
	require.Error(t, roster(l).Validate(),
		"a retraction is written by `entities retract`, never carried in the roster")
}

func TestDigestIsStableUnderKeyAndLinkReordering(t *testing.T) {
	// The digest is stamped into every row apply writes, so it has to be a
	// function of content and not of formatting. Two files that say the same
	// thing in a different order must hash the same, or the provenance link
	// between a row and a reviewed diff is noise.
	a := roster(tataSteel(), plainSplit())
	b := roster(plainSplit(), tataSteel())
	da, err := a.ComputeDigest()
	require.NoError(t, err)
	db, err := b.ComputeDigest()
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(da), hex.EncodeToString(db),
		"links are sorted by (predecessor, successor) before hashing")

	// Key order in the file is the other half: the digest is taken over the
	// canonical form, not over the bytes on disk.
	dir := t.TempDir()
	one := filepath.Join(dir, "one.json")
	two := filepath.Join(dir, "two.json")
	require.NoError(t, os.WriteFile(one, []byte(`{"version":1,"links":[
      {"reason":"succession","predecessor":"INE081A01012","successor":"INE081A01020","ticker_at_boundary":"TATASTEEL",
       "effective_from":"2022-07-29","ratified_by":"hardik","ratified_at":"2026-09-08","note":"1:10 face value split",
       "gates":{"check_digit":true,"issuer_prefix":"INE081A01","serial":"01->02","predecessor_last_bar":"2022-07-28",
                "successor_first_bar":"2022-07-29","sessions_between":0,"calendar_days_between":1,
                "gap_dates_all_settled":true,"archive_unsettled_dates":0,"boundary_close_ratio":1.0723,
                "boundary_ratio_matches":"1/1","predecessor_bars":2703,"successor_bars":1018},
       "evidence_not_gating":{"eod2_isin2hist_agrees":true,"eod2_independent":false,"eod2_predecessor_bars":0,
                "boundary_ticker_unchanged":true}}]}`), 0o644))
	// The same content, every object's keys written in a different order.
	require.NoError(t, os.WriteFile(two, []byte(`{"links":[
      {"note":"1:10 face value split","successor":"INE081A01020","predecessor":"INE081A01012",
       "evidence_not_gating":{"boundary_ticker_unchanged":true,"eod2_predecessor_bars":0,"eod2_independent":false,
                "eod2_isin2hist_agrees":true},
       "gates":{"successor_bars":1018,"predecessor_bars":2703,"boundary_ratio_matches":"1/1",
                "boundary_close_ratio":1.0723,"archive_unsettled_dates":0,"gap_dates_all_settled":true,
                "calendar_days_between":1,"sessions_between":0,"successor_first_bar":"2022-07-29",
                "predecessor_last_bar":"2022-07-28","serial":"01->02","issuer_prefix":"INE081A01","check_digit":true},
       "ratified_at":"2026-09-08","ratified_by":"hardik","effective_from":"2022-07-29",
       "ticker_at_boundary":"TATASTEEL","reason":"succession"}],"version":1}`), 0o644))

	_, d1, err := entities.Load(one)
	require.NoError(t, err)
	_, d2, err := entities.Load(two)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(d1), hex.EncodeToString(d2))
}

func TestLoadRefusesARosterWhoseStatedDigestDoesNotMatchItsContent(t *testing.T) {
	// apply stamps the roster's sha256 into every row, so the file states its
	// own digest and the loader recomputes it. A line edited after the review
	// -- the one thing the git diff cannot show, because the diff is what was
	// reviewed -- lands here.
	r := roster(tataSteel())
	d, err := r.ComputeDigest()
	require.NoError(t, err)
	r.Digest = hex.EncodeToString(d)

	dir := t.TempDir()
	path := filepath.Join(dir, "roster.json")
	write := func(r *entities.Roster) {
		b, err := json.Marshal(r)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, b, 0o644))
	}
	write(r)
	loaded, got, err := entities.Load(path)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(d), hex.EncodeToString(got))
	require.Len(t, loaded.Links, 1)

	// Now edit a line without re-digesting: the note is the most innocuous
	// field in the file and it still moves the hash.
	r.Links[0].Note = "1:10 face value split (typo fixed)"
	write(r)
	_, _, err = entities.Load(path)
	require.ErrorContains(t, err, "digest")
}

func TestLoadRejectsAnUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"links":[{"predecesor":"INE081A01012"}]}`), 0o644))
	_, _, err := entities.Load(path)
	require.Error(t, err, "a misspelled key in a hand-written line must be an error, not a silently empty field")
}

func TestCanonicalJSONFollowsRFC8785(t *testing.T) {
	// The three rules the digest depends on: keys sorted by UTF-16 code
	// unit, no insignificant whitespace, ECMAScript number formatting.
	b, err := entities.CanonicalJSON(map[string]any{
		"b": 1, "a": 2, "A": 3, "ä": 4, "€": 5,
	})
	require.NoError(t, err)
	require.Equal(t, "{\"A\":3,\"a\":2,\"b\":1,\"ä\":4,\"€\":5}", string(b))

	b, err = entities.CanonicalJSON([]any{1.0, 1.5, 0.1, 1e21, 1e-7, math0(), 1000000.0})
	require.NoError(t, err)
	require.Equal(t, `[1,1.5,0.1,1e+21,1e-7,0,1000000]`, string(b))

	b, err = entities.CanonicalJSON(map[string]any{"s": "a\"b\\c\nd\tef"})
	require.NoError(t, err)
	require.Equal(t, `{"s":"a\"b\\c\nd\tef"}`, string(b))
}

// math0 returns negative zero, which RFC 8785 serializes as "0".
func math0() float64 { z := 0.0; return -z }

// fundUnit is the hand-written line design 5.4 sends to the quarantine queue
// and design 5.6 says only a human with an NSE circular may promote: an AMC
// transfer, where the whole ISIN changes so no prefix rule can ever see it.
// The generator cannot reproduce it, this month or any month.
func fundUnit() entities.Link {
	return entities.Link{
		Reason:           entities.ReasonManual,
		Predecessor:      "INF732E01011",
		Successor:        "INF204KB14I2",
		TickerAtBoundary: "NIFTYBEES",
		EffectiveFrom:    "2019-12-20",
		Gates: entities.Gates{
			CheckDigit:         true,
			PredecessorLastBar: "2019-12-19",
			SuccessorFirstBar:  "2019-12-20",
		},
		RatifiedBy: "hardik", RatifiedAt: "2026-09-08",
		Note: "NSE circular: AMC transfer, Goldman Sachs MF to Nippon India MF",
	}
}

func TestAdoptManualCarriesHandWrittenLinesAcrossARegenerate(t *testing.T) {
	// `propose --out internal/market/entities/roster.json` is the command
	// design 9's monthly runbook names, and it rebuilds the file from the
	// generator and writes it over the target. Every `entities link` line in
	// that file is invisible to the generator by construction -- it exists
	// because no gate admitted it -- so the run deleted it, silently, and the
	// reviewer's diff showed a removal with no reason attached to it.
	generated := roster(tataSteel(), plainSplit())
	before, err := generated.ComputeDigest()
	require.NoError(t, err)

	kept, err := generated.AdoptManual([]entities.Link{fundUnit()})
	require.NoError(t, err)
	require.Len(t, kept, 1)
	require.False(t, kept[0].SupersededGenerated,
		"the generator has never proposed this pair, so nothing of its was dropped for it")
	require.Len(t, generated.Links, 3)
	require.NoError(t, generated.Validate())

	after, err := generated.ComputeDigest()
	require.NoError(t, err)
	require.NotEqual(t, before, after,
		"the digest covers the whole file, so carrying a line across is a change to it and must show as one")

	var found bool
	for _, l := range generated.Links {
		if l.Reason == entities.ReasonManual {
			found = true
			require.Equal(t, "hardik", l.RatifiedBy, "the ratifier is the whole reason the line is worth keeping")
			require.NotEmpty(t, l.Note)
		}
	}
	require.True(t, found)

	// Only `manual` lines are carried. The generated half of last month's
	// roster is NOT: the generator is the source of truth for it, and
	// carrying it across would let a line the store no longer supports
	// survive a regenerate that dropped it.
	fresh := roster(tataSteel())
	kept, err = fresh.AdoptManual([]entities.Link{plainSplit()})
	require.NoError(t, err)
	require.Empty(t, kept)
	require.Len(t, fresh.Links, 1)
}

func TestAdoptManualKeepsTheHandWrittenLineWhenTheGeneratorCatchesUp(t *testing.T) {
	// A pair a human hand-linked can become auto-acceptable later: a backfill
	// fills in the gap dates, and G4a stops refusing it. Both lines then say
	// the same thing about the same pair and only one of them names the
	// person who decided it, so the hand-written one stays and the generated
	// twin goes.
	manual := tataSteel()
	manual.Reason = entities.ReasonManual
	manual.Note = "NSE circular 2022/07: 1:10 face value split"

	generated := roster(tataSteel(), plainSplit())
	kept, err := generated.AdoptManual([]entities.Link{manual})
	require.NoError(t, err)
	require.Len(t, kept, 1)
	require.True(t, kept[0].SupersededGenerated,
		"the generator DID reproduce this pair, and the caller has to be able to say so: printing "+
			"\"the generator cannot reproduce it\" here would be a false line in the report the operator applies on")
	require.Len(t, generated.Links, 2, "one line for the pair, not two")
	require.NoError(t, generated.Validate(), "and not a G5 merge point against itself")

	for _, l := range generated.Links {
		if l.Predecessor == manual.Predecessor {
			require.Equal(t, entities.ReasonManual, l.Reason)
			require.Equal(t, "hardik", l.RatifiedBy)
		}
	}
}

// TestAdoptManualRefusesWhenTheHandWrittenBoundaryDisagreesWithTheGenerator is
// the case the supersede branch used to resolve silently and wrongly.
//
// The pair matches, so the branch above fires -- but the hand-written line
// carries a DIFFERENT effective_from from the one the generator just measured
// off bars, and the hand-written one is the line that survives. effective_from
// is what `apply` writes into symbol_links.boundary, and design 4.3 makes that
// column the one deciding which member of an entity labels a session, so every
// session between the true first bar and the stale date gets labelled as the
// predecessor. Nothing downstream catches it: `Apply`'s differsFrom refusal
// compares the PLANNED row against the stored one, and by then the plan
// already carries the stale date; for a pair not yet applied there is no
// stored row to compare against at all. The monthly roster diff shows nothing
// either, because the surviving line is byte-identical to last month's.
//
// The mechanism is the one apply.go names: bars is insert-only, but a backfill
// can add EARLIER sessions, which moves a successor's first bar. That is also
// the usual reason a hand-written pair becomes generator-acceptable in the
// first place, so the two arrive together.
func TestAdoptManualRefusesWhenTheHandWrittenBoundaryDisagreesWithTheGenerator(t *testing.T) {
	manual := tataSteel()
	manual.Reason = entities.ReasonManual
	manual.Note = "NSE circular 2022/07: 1:10 face value split"
	manual.EffectiveFrom = "2022-08-12" // the generator now measures 2022-07-29
	manual.Gates.SuccessorFirstBar = "2022-08-12"

	_, err := roster(tataSteel(), plainSplit()).AdoptManual([]entities.Link{manual})
	require.ErrorContains(t, err, "disagree about the boundary")
	require.ErrorContains(t, err, "effective_from")
	require.ErrorContains(t, err, "2022-08-12", "the hand-written date has to be named")
	require.ErrorContains(t, err, "2022-07-29", "and so does the measured one, because the operator chooses between them")
	require.ErrorContains(t, err, "--discard-manual")

	// A predecessor_last_bar that moved is the same defect arriving from the
	// other side of the gap, and it is refused for the same reason.
	moved := tataSteel()
	moved.Reason = entities.ReasonManual
	moved.Note = "NSE circular 2022/07"
	moved.Gates.PredecessorLastBar = "2022-06-30"
	_, err = roster(tataSteel(), plainSplit()).AdoptManual([]entities.Link{moved})
	require.ErrorContains(t, err, "gates.predecessor_last_bar")

	// And the agreeing case still supersedes: the refusal is about the
	// boundary, not about the pair matching.
	agrees := tataSteel()
	agrees.Reason = entities.ReasonManual
	agrees.Note = "NSE circular 2022/07"
	kept, err := roster(tataSteel(), plainSplit()).AdoptManual([]entities.Link{agrees})
	require.NoError(t, err)
	require.Len(t, kept, 1)
	require.True(t, kept[0].SupersededGenerated)
}

func TestAdoptManualRefusesWhenAHandWrittenLineContradictsAGeneratedOne(t *testing.T) {
	// The case that must not be resolved by machine: two lines claiming one
	// endpoint, one of them written by a human. Validate would catch it as a
	// G5 graph error after the fact; refusing here names the two lines
	// instead, because the operator has to decide which is wrong.
	rival := fundUnit()
	rival.Predecessor = "INE090A01013" // plainSplit's predecessor
	rival.Gates.PredecessorLastBar = "2019-12-19"
	_, err := roster(tataSteel(), plainSplit()).AdoptManual([]entities.Link{rival})
	require.ErrorContains(t, err, "both claim INE090A01013 as a predecessor")
	require.ErrorContains(t, err, "--discard-manual")

	rival = fundUnit()
	rival.Successor = "INE090A01021" // plainSplit's successor
	rival.Gates.SuccessorFirstBar = "2019-12-20"
	_, err = roster(tataSteel(), plainSplit()).AdoptManual([]entities.Link{rival})
	require.ErrorContains(t, err, "both claim INE090A01021 as a successor")
}
