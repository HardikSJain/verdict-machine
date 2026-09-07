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

	gaps, queue := entities.UnlinkedCandidates(
		[]entities.Candidate{linked, unlinked, queued, queuedButLinked}, entityOf)

	require.Len(t, gaps, 1)
	require.Equal(t, "INE090A01021", gaps[0].Successor,
		"an accepted candidate the map does not carry is the seed falling behind the archive, and it is what the monthly item exists to catch")
	require.Len(t, queue, 1)
	require.Equal(t, "INF204KB14I2", queue[0].Successor,
		"a quarantined pair is reported and is not a defect: 181 exist today, §10 says they will not be worked, and a permanently red check is one nobody reads")
}

func TestUnlinkedCandidates_TreatsAnUnknownSymbolAsUnlinked(t *testing.T) {
	// Neither symbol is in the map -- a pair registered after the pin the
	// check ran at. Both would read as entity 0 from a bare map lookup,
	// 0 == 0, and the pair would be reported as ALREADY LINKED: the one
	// wrong answer this function can give, and it would give it silently.
	gaps, _ := entities.UnlinkedCandidates(
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
	gaps, queue := entities.UnlinkedCandidates(cands, before)
	require.Len(t, gaps, 1, "the gates accept this succession and the store has never been told about it")
	require.Equal(t, calSucc, gaps[0].Successor)
	require.Empty(t, queue)

	digest, err := roster.ComputeDigest()
	require.NoError(t, err)
	_, err = entities.Apply(ctx, pool, roster, digest, "stage3 test", false)
	require.NoError(t, err)

	after, err := store.EntityMapAt(ctx, time.Now())
	require.NoError(t, err)
	gaps, _ = entities.UnlinkedCandidates(cands, after)
	require.Empty(t, gaps,
		"once the link is in the map the finding is closed; a monthly item that stays red after the fix is one an operator stops reading")
}
