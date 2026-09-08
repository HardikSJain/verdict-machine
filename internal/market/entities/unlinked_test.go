package entities_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/entities"
)

// The candidate half of `verdict entities check` -- the thing that makes the
// monthly ops item able to go red.
//
// Design §4.6 gives check two jobs: the three invariants, and "unlinked
// succession candidates that have appeared since the last seed". Stage 1
// shipped only the first, because the generator did not exist yet, and said
// so in docs/DESIGN.md. Stage 2 built the generator. This is the join, and
// without it the monthly item is a calendar entry rather than a control: an
// unlinked succession produces no error, no missing row and no violation --
// only a universe that is quietly short one large cap for six months.

func candidateFixture(pred, succ string, predID, succID int64, failed ...string) entities.Candidate {
	return entities.Candidate{
		Predecessor: pred, Successor: succ,
		PredecessorID: predID, SuccessorID: succID,
		TickerAfter:  "TATASTEEL",
		SuccessorSpn: [2]string{"2022-07-29", "2026-09-04"},
		FailedGates:  failed,
	}
}

func TestUnlinkedCandidates_SeparatesTheSeedGapFromTheQuarantineQueue(t *testing.T) {
	linked := candidateFixture(calPred, calSucc, 10, 11)
	unlinked := candidateFixture("INE090A01013", "INE090A01021", 20, 21)
	queued := candidateFixture("INF732E01011", "INF204KB14I2", 30, 31, entities.G0)
	queuedButLinked := candidateFixture("INE528G01027", "INE528G01035", 40, 41, entities.G6)

	// 11 already resolves to 10 and 41 to 40; the other two pairs stand apart.
	entityOf := map[int64]int64{10: 10, 11: 10, 20: 20, 21: 21, 30: 30, 31: 31, 40: 40, 41: 40}

	gaps, queue, stale := entities.UnlinkedCandidates(
		[]entities.Candidate{linked, unlinked, queued, queuedButLinked}, entityOf)

	require.Len(t, gaps, 1)
	require.Equal(t, "INE090A01021", gaps[0].Successor,
		"an accepted candidate the map does not carry is the seed falling behind the archive, and it is what the monthly item exists to catch")
	require.Len(t, queue, 1)
	require.Equal(t, "INF204KB14I2", queue[0].Successor,
		"a quarantined pair is reported and is not a defect: 181 exist today, §10 says they will not be worked, and a permanently red check is one nobody reads")

	// The third category, and the one an earlier version dropped on the
	// floor: the map DOES carry 40 -> 41 and the gates now reject it. It is
	// neither a seed gap nor a queue entry, so a two-way split reported it
	// nowhere at all -- and §5.6 says there is no other detector.
	require.Len(t, stale, 1)
	require.Equal(t, "INE528G01035", stale[0].Successor,
		"design 5.5 rests on `entities check` re-evaluating the gates against the store; a merged pair that stopped passing them has to come back from this function")
	require.Equal(t, []string{entities.G6}, stale[0].FailedGates,
		"which gate stopped passing is the whole finding: the operator decides on it")
}

// TestUnlinkedCandidates_ReChecksTheGatesAgainstLinksTheMapAlreadyHolds is the
// re-check leg against a real store, through the real generator and the real
// writer.
//
// Design §5.5 offers exactly two things that make ratifying 444 irreversible
// merges wholesale acceptable, and the second is that "every gate is
// independently re-checkable (`verdict entities check` re-evaluates them
// against the store)". It did not: the linked branch returned before
// Accepted() was ever consulted. The class that goes missing is the one G4
// exists for -- §5.3 calls it "the gate that kills demergers" and names Tube
// Investments as a real pair that would have merged two economically
// different securities -- and G4 counts sessions out of `bars`, so a backfill
// filling a hole inside a boundary gap turns a standing link from accepted to
// rejected without touching one row of symbol_links.
//
// This test applies the link while the gap is empty, then adds the sessions
// that were missing from it, exactly as `backfill` would.
func TestUnlinkedCandidates_ReChecksTheGatesAgainstLinksTheMapAlreadyHolds(t *testing.T) {
	ctx := context.Background()
	store, pool := proposeStore(t)
	// A three-calendar-day gap, so G4b's cap is never the thing deciding
	// this: Monday 07-31 to Thursday 08-03, with two weekdays inside it that
	// the archive has not fetched yet.
	predLast := market.Day(2017, 7, 31)
	succFirst := market.Day(2017, 8, 3)
	unfetched := []time.Time{market.Day(2017, 8, 1), market.Day(2017, 8, 2)}
	var fetched []time.Time
	for _, d := range weekdaysIn(archiveFrom, archiveTo) {
		if d.Equal(unfetched[0]) || d.Equal(unfetched[1]) {
			continue
		}
		fetched = append(fetched, d)
	}
	series(t, store, calCtrl, "CONTROL", fetched, 100, 100)
	series(t, store, calPred, "TATASTEEL", weekdaysIn(archiveFrom, predLast), 959.4, 100.35)
	series(t, store, calSucc, "TATASTEEL", weekdaysIn(succFirst, archiveTo), 107.6, 110)
	settle(t, pool, lateFetch)

	// The gap holds no bars from any symbol, so G4 counts zero sessions
	// between and the pair is accepted. That is the hazard §5.3 records
	// rather than a contrived state: `ingest_log` held an hour of zero bars
	// on the real session 2020-07-13, and `data/backfill-weekends.log` added
	// 19 sessions that were in neither `bars` nor `ingest_log`.
	roster, cands, _ := propose(t, pool)
	require.Len(t, roster.Links, 1)
	accepted := candidateFor(t, cands, calPred, calSucc)
	require.True(t, accepted.Accepted())

	digest, err := roster.ComputeDigest()
	require.NoError(t, err)
	_, err = entities.Apply(ctx, pool, roster, digest, "stage3 test", false)
	require.NoError(t, err)

	// A backfill now fills the gap: the control name traded through those two
	// sessions all along and they were simply never fetched.
	series(t, store, calCtrl, "CONTROL", unfetched, 100, 100)

	_, cands, _ = propose(t, pool)
	require.Contains(t, candidateFor(t, cands, calPred, calSucc).FailedGates, entities.G4,
		"two live sessions now sit in the gap, and G4 is the gate that kills demergers")

	entityOf, err := store.EntityMapAt(ctx, time.Now())
	require.NoError(t, err)
	gaps, queue, stale := entities.UnlinkedCandidates(cands, entityOf)
	require.Empty(t, gaps, "the map carries this pair, so it is not a seed gap")
	require.Empty(t, queue, "and it is not an unworked queue entry either -- it is already merged")
	require.Len(t, stale, 1,
		"a standing merge that stopped passing its own gates is the one thing §5.5 promises `check` can see, and it must not vanish between the two other categories")
	require.Equal(t, calSucc, stale[0].Successor)
	require.Contains(t, stale[0].FailedGates, entities.G4)
}

func TestUnlinkedCandidates_TreatsAnUnknownSymbolAsUnlinked(t *testing.T) {
	// Neither symbol is in the map -- a pair registered after the pin the
	// check ran at. Both would read as entity 0 from a bare map lookup,
	// 0 == 0, and the pair would be reported as ALREADY LINKED: the one
	// wrong answer this function can give, and it would give it silently.
	gaps, _, _ := entities.UnlinkedCandidates(
		[]entities.Candidate{candidateFixture(calPred, calSucc, 10, 11)}, map[int64]int64{})
	require.Len(t, gaps, 1, "a symbol the pinned map has never heard of is not evidence that a link exists")
}

// TestUnlinkedCandidates_GoesQuietOnceTheRosterIsApplied is the same check
// run against a real store, through the real generator and the real writer,
// with the map read through the published entity_map_at.
//
// It asserts the loop the ops item is: propose finds a succession, check
// reports it because the map does not carry it, a human applies the roster,
// and the next check is silent. Both halves matter. A check that cannot fire
// is not a control; a check that keeps firing after the link is written would
// be treated as noise within two months.
func TestUnlinkedCandidates_GoesQuietOnceTheRosterIsApplied(t *testing.T) {
	ctx := context.Background()
	store, pool := proposeStore(t)
	series(t, store, calCtrl, "CONTROL", weekdaysIn(archiveFrom, archiveTo), 100, 100)
	series(t, store, calPred, "TATASTEEL", weekdaysIn(archiveFrom, market.Day(2017, 7, 28)), 959.4, 100.35)
	series(t, store, calSucc, "TATASTEEL", weekdaysIn(market.Day(2017, 7, 31), archiveTo), 107.6, 110)
	settle(t, pool, lateFetch)

	roster, cands, _ := propose(t, pool)
	require.Len(t, roster.Links, 1)

	before, err := store.EntityMapAt(ctx, time.Now())
	require.NoError(t, err)
	gaps, queue, stale := entities.UnlinkedCandidates(cands, before)
	require.Len(t, gaps, 1, "the gates accept this succession and the store has never been told about it")
	require.Equal(t, calSucc, gaps[0].Successor)
	require.Empty(t, queue)
	require.Empty(t, stale, "nothing is linked yet, so no standing link can have stopped passing")

	digest, err := roster.ComputeDigest()
	require.NoError(t, err)
	_, err = entities.Apply(ctx, pool, roster, digest, "stage3 test", false)
	require.NoError(t, err)

	after, err := store.EntityMapAt(ctx, time.Now())
	require.NoError(t, err)
	gaps, _, stale = entities.UnlinkedCandidates(cands, after)
	require.Empty(t, gaps,
		"once the link is in the map the finding is closed; a monthly item that stays red after the fix is one an operator stops reading")
	require.Empty(t, stale,
		"and the pair still passes every gate, so the re-check leg has nothing to say about it either")
}
