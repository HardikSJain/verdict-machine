package entities_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/entities"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// The candidate generator, end to end against a real store.
//
// Design 5.5: the reviewer of the roster PR is checking the GENERATOR, so a
// regression in `propose` is ratified wholesale over 444 irreversible rows.
// `Roster.Validate` cannot catch one -- it re-reads the numbers the generator
// itself wrote -- so these tests run the measurement against bars and
// ingest_log and assert on what comes back.

const (
	tubePred = "INE149A01025" // Tube Investments, to 2017-08-23
	tubeSucc = "INE149A01033" // and the post-demerger company, from 2017-10-10
	calPred  = "INE081A01012"
	calSucc  = "INE081A01020"
	calCtrl  = "INE666D01014"
)

// archiveFrom/archiveTo bound every fixture below: one synthetic trading year
// whose calendar is plain weekdays, because what is under test is the gates
// and not NSE's holiday list.
var (
	archiveFrom = market.Day(2017, 1, 2)
	archiveTo   = market.Day(2017, 12, 29)
	// lateFetch is after every date in the span has passed its settlement
	// horizon, so an unlogged date inside the span is a HOLE rather than a
	// pending tail.
	lateFetch = time.Date(2018, 1, 5, 10, 0, 0, 0, time.UTC)
)

func weekdaysIn(from, to time.Time) []time.Time {
	var out []time.Time
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		out = append(out, d)
	}
	return out
}

// series writes one symbol's bars over the given sessions at a flat close,
// with the first session's close overridden so a boundary ratio can be aimed.
func series(t *testing.T, store *market.Store, isin, ticker string, days []time.Time, firstClose, close float64) {
	t.Helper()
	var bars []market.Bar
	for i, d := range days {
		c := close
		if i == 0 {
			c = firstClose
		}
		bars = append(bars, bar(isin, ticker, d, c, 5_000_000))
	}
	_, err := store.InsertBars(context.Background(), market.SourceBhavcopy, bars)
	require.NoError(t, err)
}

// settle logs every calendar date in the span, except the ones named, at a
// chosen fetched_at. The explicit timestamp is what makes the hole/pending
// split testable without depending on the wall clock: archiveHoles compares
// each date's settlement horizon against the last fetch for the source.
func settle(t *testing.T, pool *pgxpool.Pool, fetchedAt time.Time, skip ...time.Time) {
	t.Helper()
	unlogged := map[time.Time]bool{}
	for _, d := range skip {
		unlogged[d] = true
	}
	for d := archiveFrom; !d.After(archiveTo); d = d.AddDate(0, 0, 1) {
		if unlogged[d] {
			continue
		}
		status := "ok"
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			status = "no-file"
		}
		_, err := pool.Exec(context.Background(),
			`INSERT INTO ingest_log (source, date, status, rows, fetched_at) VALUES ($1, $2, $3, 1, $4)`,
			market.SourceBhavcopy, d, status, fetchedAt)
		require.NoError(t, err)
	}
}

func proposeStore(t *testing.T) (*market.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.Pool(t)
	return market.NewStore(pool), pool
}

func propose(t *testing.T, pool *pgxpool.Pool) (*entities.Roster, []entities.Candidate, *entities.Counts) {
	t.Helper()
	r, cands, counts, err := entities.Propose(context.Background(), pool, entities.ProposeOptions{})
	require.NoError(t, err)
	return r, cands, counts
}

func candidateFor(t *testing.T, cands []entities.Candidate, pred, succ string) entities.Candidate {
	t.Helper()
	for _, c := range cands {
		if c.Predecessor == pred && c.Successor == succ {
			return c
		}
	}
	t.Fatalf("no candidate %s -> %s in %d candidates", pred, succ, len(cands))
	return entities.Candidate{}
}

func TestProposeAcceptsACleanSplitAndQuarantinesNothingElse(t *testing.T) {
	store, pool := proposeStore(t)
	series(t, store, calCtrl, "CONTROL", weekdaysIn(archiveFrom, archiveTo), 100, 100)
	series(t, store, calPred, "TATASTEEL", weekdaysIn(archiveFrom, market.Day(2017, 7, 28)), 959.4, 100.35)
	series(t, store, calSucc, "TATASTEEL", weekdaysIn(market.Day(2017, 7, 31), archiveTo), 107.6, 110)
	settle(t, pool, lateFetch)

	r, cands, counts := propose(t, pool)
	require.Equal(t, 1, counts.Candidates)
	require.Equal(t, 1, counts.Accepted)
	require.Zero(t, counts.Quarantined)
	require.True(t, counts.LinkOverlayPresent)
	require.Equal(t, archiveFrom, counts.ArchiveFrom)
	require.Equal(t, archiveTo, counts.ArchiveTo)

	require.NoError(t, r.Validate(), "the generated roster must pass the gates it claims to have run")
	require.Len(t, r.Links, 1)
	l := r.Links[0]
	require.Equal(t, entities.ReasonSuccession, l.Reason)
	require.Equal(t, "2017-07-31", l.EffectiveFrom)
	require.Equal(t, "2017-07-28", l.Gates.PredecessorLastBar)
	require.Zero(t, l.Gates.SessionsBetween)
	require.Equal(t, 3, l.Gates.CalendarDaysBetween, "Friday to Monday")
	require.True(t, l.Gates.GapDatesAllSettled)
	require.Equal(t, "1/1", l.Gates.BoundaryRatioMatches, "107.60 / 100.35, the TATASTEEL mode of the bimodal distribution")
	require.Empty(t, l.RatifiedBy, "propose cannot know who will ratify, and filling it in would be the machine asserting a human's approval")

	c := candidateFor(t, cands, calPred, calSucc)
	require.Equal(t, "accepted", c.Disposition)
	require.Zero(t, c.OverlapDates)
	require.Equal(t, "issuer-prefix", c.Generator)

	// The review file carries one record per candidate, accepted or not: it
	// is the only material in the whole process a reviewer can disagree with.
	var buf bytes.Buffer
	require.NoError(t, entities.WriteReview(&buf, cands))
	var back entities.Candidate
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &back))
	require.Equal(t, c.Successor, back.Successor)
	require.Nil(t, back.DeliveryRatio,
		"bhavcopy's parser never reads DELIV_QTY, so the delivery ratio is null on every record; see docs/DESIGN.md")
}

func TestProposeQuarantinesADemergerOnG4(t *testing.T) {
	// The gate that earns its keep on a real pair: INE149A01025 ->
	// INE149A01033, the 2017 Tube Investments demerger. It is disjoint, it
	// passes G0, G1, G2 and G3, and only the live sessions in its gap stop it
	// from merging two economically different securities.
	store, pool := proposeStore(t)
	series(t, store, calCtrl, "CONTROL", weekdaysIn(archiveFrom, archiveTo), 100, 100)
	series(t, store, tubePred, "TIINDIA", weekdaysIn(archiveFrom, market.Day(2017, 8, 23)), 200, 200)
	series(t, store, tubeSucc, "TIINDIA", weekdaysIn(market.Day(2017, 10, 10), archiveTo), 100, 100)
	settle(t, pool, lateFetch)

	r, cands, counts := propose(t, pool)
	require.Empty(t, r.Links, "nothing may be auto-accepted here")
	require.Equal(t, 1, counts.Quarantined)
	c := candidateFor(t, cands, tubePred, tubeSucc)
	require.Equal(t, "quarantined", c.Disposition)
	require.Equal(t, "G4 adjacent", c.FailedGates[0])
	require.Equal(t, 33, c.Gates.SessionsBetween, "the sessions the control name holds inside the gap")
	require.True(t, c.Gates.GapDatesAllSettled, "the archive is complete; it is the sessions themselves that refuse this")
}

func TestProposeQuarantinesALongGapEvenWithNoSessionsInIt(t *testing.T) {
	// G4b, and this is the case that makes it worth having. Here the gap
	// holds no bars at all -- the archive was never asked for those dates, or
	// they were fetched and lost -- while ingest_log says every one of them
	// settled 'ok'. G4 counts zero sessions between and G4a is satisfied, so
	// the calendar cap is the only thing left standing between a six-week gap
	// and an irreversible merge.
	store, pool := proposeStore(t)
	gapStart, gapEnd := market.Day(2017, 8, 24), market.Day(2017, 10, 9)
	var control []time.Time
	for _, d := range weekdaysIn(archiveFrom, archiveTo) {
		if !d.Before(gapStart) && !d.After(gapEnd) {
			continue
		}
		control = append(control, d)
	}
	series(t, store, calCtrl, "CONTROL", control, 100, 100)
	series(t, store, tubePred, "TIINDIA", weekdaysIn(archiveFrom, market.Day(2017, 8, 23)), 100, 100)
	series(t, store, tubeSucc, "TIINDIA", weekdaysIn(market.Day(2017, 10, 10), archiveTo), 100, 100)
	settle(t, pool, lateFetch)

	r, cands, counts := propose(t, pool)
	require.Empty(t, r.Links)
	require.Zero(t, counts.ArchiveUnsettled)
	c := candidateFor(t, cands, tubePred, tubeSucc)
	require.Zero(t, c.Gates.SessionsBetween, "bars says there was nothing in the gap")
	require.True(t, c.Gates.GapDatesAllSettled, "and ingest_log says the archive settled every date in it")
	require.Equal(t, []string{"G4b calendar cap"}, c.FailedGates,
		"so the belt-and-braces cap is the only gate left, which is exactly why it is independent of bars")
}

func TestProposeRefusesWhileTheArchiveHasAHole(t *testing.T) {
	// G4a's archive-wide refusal. A real NSE session the store has not
	// fetched reads as "zero sessions between", and the repo's own record
	// shows the hazard is not hypothetical: 2020-07-13 was logged 'error'
	// twice and only 'ok' an hour later, and a weekend backfill later added
	// 19 sessions that were in neither bars nor ingest_log.
	store, pool := proposeStore(t)
	series(t, store, calCtrl, "CONTROL", weekdaysIn(archiveFrom, archiveTo), 100, 100)
	series(t, store, calPred, "TATASTEEL", weekdaysIn(archiveFrom, market.Day(2017, 7, 28)), 100, 100)
	series(t, store, calSucc, "TATASTEEL", weekdaysIn(market.Day(2017, 7, 31), archiveTo), 100, 100)
	settle(t, pool, lateFetch, market.Day(2017, 5, 10))

	r, cands, counts, err := entities.Propose(context.Background(), pool, entities.ProposeOptions{})
	require.Error(t, err)
	require.ErrorContains(t, err, "2017-05-10")
	require.ErrorContains(t, err, "unsettled")
	require.Nil(t, r, "not one auto-accept line may be emitted from an archive with a hole in it")
	require.Nil(t, cands)
	require.Equal(t, 1, counts.ArchiveUnsettled, "the counts still come back, so the operator can see how bad it is")
	require.Zero(t, counts.ArchivePending)
}

func TestProposeRunsThroughAPendingTailAndQuarantinesTheCandidateInIt(t *testing.T) {
	// The other half of the same rule. backfill.noFileSettled deliberately
	// leaves a recent date unlogged rather than writing a terminal 'no-file'
	// for a session NSE may not have published yet, so the tail of the
	// archive always carries a day or two of unlogged dates. Treating that as
	// a hole would make the roster ungeneratable for a reason that is a clock
	// rather than a gap -- but the date is still not settled for a candidate
	// whose own boundary lands in it.
	store, pool := proposeStore(t)
	tail := market.Day(2017, 12, 28)
	var control []time.Time
	for _, d := range weekdaysIn(archiveFrom, archiveTo) {
		if d.Equal(tail) {
			continue
		}
		control = append(control, d)
	}
	series(t, store, calCtrl, "CONTROL", control, 100, 100)
	series(t, store, calPred, "TATASTEEL", weekdaysIn(archiveFrom, market.Day(2017, 12, 27)), 100, 100)
	series(t, store, calSucc, "TATASTEEL", []time.Time{archiveTo}, 100, 100)
	// The last fetch ran before this date's settlement horizon, so no run has
	// had its chance at it yet.
	settle(t, pool, time.Date(2017, 12, 29, 10, 0, 0, 0, time.UTC), tail)

	r, cands, counts := propose(t, pool)
	require.Zero(t, counts.ArchiveUnsettled, "a pending tail is not a hole and must not refuse the whole run")
	require.Equal(t, 1, counts.ArchivePending)
	require.Empty(t, r.Links)
	c := candidateFor(t, cands, calPred, calSucc)
	require.False(t, c.Gates.GapDatesAllSettled)
	require.Equal(t, []string{"G4a settled calendar"}, c.FailedGates,
		"a boundary that straddles an unsettled date is quarantined even though the run itself is fine")
}

func TestProposeMeasuresTheOverlapRatherThanInferringItFromTheEndpoints(t *testing.T) {
	// G3's real claim is "no date is held by both"; strict span ordering only
	// implies it. A candidate whose spans interleave is the catastrophic case
	// and is worth measuring rather than deducing, so the count comes from an
	// INTERSECT over bars.
	store, pool := proposeStore(t)
	series(t, store, calCtrl, "CONTROL", weekdaysIn(archiveFrom, archiveTo), 100, 100)
	series(t, store, calPred, "TATASTEEL", weekdaysIn(archiveFrom, archiveTo), 100, 100)
	shared := weekdaysIn(market.Day(2017, 3, 1), archiveTo)
	series(t, store, calSucc, "TATASTEEL", shared, 100, 100)
	settle(t, pool, lateFetch)

	_, cands, _ := propose(t, pool)
	c := candidateFor(t, cands, calPred, calSucc)
	require.Equal(t, len(shared), c.OverlapDates,
		"every session both members hold is counted, off bars and not off the span endpoints")
	require.Contains(t, c.FailedGates, "G3 disjoint")
	require.Equal(t, "G3 disjoint", c.FailedGates[0])
}

func TestProposeQuarantinesAPairAHumanAlreadyRetracted(t *testing.T) {
	// Design 5.6's undo has to survive the design's own monthly ops loop.
	// `propose` reads bars, symbols and ingest_log, so a retracted pair passes
	// every gate again next month and is regenerated into the roster file --
	// which `propose` overwrites wholesale -- and `apply` re-links it without
	// a word, because a retraction row sets entity_id = symbol_id and the
	// "already linked" skip cannot see it. Then the undo survives only if a
	// human remembers to hand-delete a line from a machine-generated file
	// every month, forever.
	ctx := context.Background()
	store, pool := proposeStore(t)
	series(t, store, calCtrl, "CONTROL", weekdaysIn(archiveFrom, archiveTo), 100, 100)
	series(t, store, calPred, "TATASTEEL", weekdaysIn(archiveFrom, market.Day(2017, 7, 28)), 100, 100)
	series(t, store, calSucc, "TATASTEEL", weekdaysIn(market.Day(2017, 7, 31), archiveTo), 100, 100)
	settle(t, pool, lateFetch)

	r, _, counts := propose(t, pool)
	require.Equal(t, 1, counts.Accepted)
	digest, err := r.ComputeDigest()
	require.NoError(t, err)
	_, err = entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)

	ids := symbolIDs(t, pool)
	_, err = entities.Retract(ctx, pool, itoa(ids[calPred]), "these are two different companies", "", false)
	require.NoError(t, err)

	again, cands, counts := propose(t, pool)
	require.Empty(t, again.Links, "the regenerated roster must not carry a link a human withdrew")
	require.Equal(t, 1, counts.RetractedPairs)
	require.True(t, counts.LinkOverlayPresent)
	c := candidateFor(t, cands, calPred, calSucc)
	require.Equal(t, []string{entities.QRetracted}, c.FailedGates,
		"every gate still passes; what stops it is that a human already said no")
	require.NotEmpty(t, c.RetractedAt)
	require.Equal(t, "these are two different companies", c.RetractionNote,
		"and the reviewer is told why, in the words of whoever withdrew it")

	// It is a quarantine and not a refusal: design 8.4 requires the re-link to
	// stay reachable. A human who still wants it writes it by hand.
	_, err = entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
}

func TestApplyWarnsWhenItRelinksARetractedPair(t *testing.T) {
	// The other half of the same defect. `apply` must still be able to write
	// the link back -- design 8.4 executes exactly that -- but never by
	// accident, so the row it writes says it is undoing an undo.
	ctx := context.Background()
	_, pool := tataSteelFixture(t)
	r, digest := tataSteelRoster(t)
	_, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)

	ids := symbolIDs(t, pool)
	_, err = entities.Retract(ctx, pool, itoa(ids[predISIN]), "wrong", "", false)
	require.NoError(t, err)

	res, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	require.True(t, res.Rows[0].WasRetracted,
		"this row re-links a pair a human withdrew, and the operator has to be told")
}

func TestApplyDoesNotWarnOnAFirstLink(t *testing.T) {
	// The other side of the same flag: a warning printed on every run is a
	// warning nobody reads. (Its own test rather than a second half, because
	// testutil.Pool holds a database-wide advisory lock for the test's whole
	// lifetime and two fixtures in one test deadlock against each other.)
	_, pool := tataSteelFixture(t)
	r, digest := tataSteelRoster(t)
	res, err := entities.Apply(context.Background(), pool, r, digest, "", false)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	require.False(t, res.Rows[0].WasRetracted)
}

func TestEvaluatePairRecordsTheGatesWithoutEnforcingThem(t *testing.T) {
	// `entities link` is how a QUARANTINED pair gets into the map, so the
	// gates are recorded rather than enforced: refusing to write down what
	// they said would leave the riskiest row in the table as the only one
	// carrying no measurement at all.
	store, pool := proposeStore(t)
	series(t, store, calCtrl, "CONTROL", weekdaysIn(archiveFrom, archiveTo), 100, 100)
	series(t, store, tubePred, "TIINDIA", weekdaysIn(archiveFrom, market.Day(2017, 8, 23)), 200, 200)
	series(t, store, tubeSucc, "TIINDIA", weekdaysIn(market.Day(2017, 10, 10), archiveTo), 100, 100)
	settle(t, pool, lateFetch)

	c, err := entities.EvaluatePair(context.Background(), pool, tubePred, tubeSucc, time.Time{})
	require.NoError(t, err)
	require.Equal(t, "manual", c.Generator)
	require.Equal(t, 33, c.Gates.SessionsBetween)
	require.Equal(t, "2017-10-10", c.Gates.SuccessorFirstBar)
	require.Contains(t, c.FailedGates, "G4 adjacent", "the failures are on the line, not hidden by it")

	_, err = entities.EvaluatePair(context.Background(), pool, "INE000A01018", tubeSucc, time.Time{})
	require.ErrorContains(t, err, "no nse-bhavcopy bars in this store")
}

func TestAppendManualWritesTheLineAndRefusesADuplicate(t *testing.T) {
	// No database: `entities link` edits the file a reviewer will read and
	// writes nothing to the store, because identity is decided in git.
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.json")
	c := &entities.Candidate{
		Predecessor:  "INF732E01011",
		Successor:    "INF204KB14I2",
		TickerAfter:  "NIFTYBEES",
		SuccessorSpn: [2]string{"2019-12-20", "2026-09-07"},
		Gates: entities.Gates{
			CheckDigit:         true,
			PredecessorLastBar: "2019-12-19",
			SuccessorFirstBar:  "2019-12-20",
		},
		FailedGates: []string{"G0 well-formed", "G1 issuer identity"},
	}
	r, err := entities.AppendManual(path, c, "hardik", "NSE circular: AMC transfer")
	require.NoError(t, err)
	require.Len(t, r.Links, 1)
	require.Equal(t, entities.ReasonManual, r.Links[0].Reason)

	// It round-trips through Load, which is what `apply` will do to it: the
	// file states its own digest and the gates run again against it.
	loaded, digest, err := entities.Load(path)
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	require.Equal(t, "hardik", loaded.Links[0].RatifiedBy)
	require.Equal(t, "2019-12-20", loaded.Links[0].EffectiveFrom)

	_, err = entities.AppendManual(path, c, "hardik", "again")
	require.ErrorContains(t, err, "already in")

	// And a hand-written line with a typo in an ISIN is refused before it can
	// be written, which is the one thing G0 is for.
	bad := *c
	bad.Successor = "INE090A01014" // one digit off a real ISIN
	_, err = entities.AppendManual(filepath.Join(dir, "other.json"), &bad, "hardik", "typo")
	require.ErrorContains(t, err, "check digit")
	_, err = os.Stat(filepath.Join(dir, "other.json"))
	require.True(t, os.IsNotExist(err), "a refused line leaves no file behind")
}

func TestTheCommittedRosterLoadsAndValidates(t *testing.T) {
	// The artefact being shipped, checked without a database, so
	// `go test -short ./...` covers it. It was previously only ever loaded
	// inside the 8.14 fixture, which skips under -short.
	r, digest, err := entities.Load("roster.json")
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	require.Len(t, r.Links, 444)
	for _, l := range r.Links {
		require.Equal(t, entities.ReasonSuccession, l.Reason, "%s -> %s", l.Predecessor, l.Successor)
	}
}
