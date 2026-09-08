package entities_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/entities"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// apply's refusals. Every one of them is a case where writing the row would
// be worse than failing, and symbol_links has no DELETE to fix it afterwards.

func TestApplyDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	_, pool := tataSteelFixture(t)
	r, digest := tataSteelRoster(t)

	res, err := entities.Apply(ctx, pool, r, digest, "", true)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1, "a dry run still plans every row, so the operator sees what would land")
	require.True(t, res.DryRun)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*)::int FROM symbol_links`).Scan(&n))
	require.Zero(t, n, "and rolls back, because the roster PR is reviewed before the rows exist")
}

func TestApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	_, pool := tataSteelFixture(t)
	r, digest := tataSteelRoster(t)

	_, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
	again, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
	require.Empty(t, again.Rows)
	require.Len(t, again.Skipped, 1, "a symbol that already resolves to the intended entity is skipped")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*)::int FROM symbol_links`).Scan(&n))
	require.Equal(t, 1, n,
		"re-running a monthly ops item must not double a table that cannot be pruned")
}

func TestApplyRefusesToRepointAnExistingMember(t *testing.T) {
	// Moving a member to a different entity is a decision with a name --
	// retract, then apply -- and doing it silently inside apply would let a
	// roster edit move a company between entities with nothing recording
	// that the previous answer was withdrawn.
	ctx := context.Background()
	_, pool := tataSteelFixture(t)
	r, digest := tataSteelRoster(t)
	_, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)

	// The re-point has to be a MANUAL line to get this far: the structural
	// gates would refuse a cross-issuer succession long before apply looked
	// at the store, which is itself the first line of defence.
	moved := entities.Link{
		Reason:        entities.ReasonManual,
		Predecessor:   ctrlISIN,
		Successor:     succISIN,
		EffectiveFrom: "2022-07-29",
		Gates: entities.Gates{
			CheckDigit:         true,
			PredecessorLastBar: "2022-07-28",
			SuccessorFirstBar:  "2022-07-29",
		},
		RatifiedBy: "hardik", RatifiedAt: "2026-09-08",
		Note: "a deliberate re-point, which apply must still refuse",
	}
	other := &entities.Roster{Version: 1, Links: []entities.Link{moved}}
	d2, err := other.ComputeDigest()
	require.NoError(t, err)

	_, err = entities.Apply(ctx, pool, other, d2, "", false)
	require.ErrorContains(t, err, "already resolves to entity")
	require.ErrorContains(t, err, "retract the existing link first")
}

func TestApplyRefusesAnISINTheStoreHasNeverSeen(t *testing.T) {
	// A link for a company with no bars is a row asserting something nothing
	// can contradict: invisible to every read, uncheckable by
	// `entities check`, and permanent.
	ctx := context.Background()
	_, pool := tataSteelFixture(t)
	l := tataSteel()
	l.Successor = "INE081A01038"
	l.Gates.Serial = "01->03"
	r := &entities.Roster{Version: 1, Links: []entities.Link{l}}
	d, err := r.ComputeDigest()
	require.NoError(t, err)

	_, err = entities.Apply(ctx, pool, r, d, "", false)
	require.ErrorContains(t, err, "not registered in this store")
	require.ErrorContains(t, err, "INE081A01038")
}

func TestApplyRefusesWithoutADigest(t *testing.T) {
	ctx := context.Background()
	_, pool := tataSteelFixture(t)
	r, _ := tataSteelRoster(t)
	_, err := entities.Apply(ctx, pool, r, nil, "", false)
	require.ErrorContains(t, err, "roster digest",
		"roster_sha is the only thing pointing an irreversible merge back at a reviewed diff")
}

func TestRetractRefusesAnUnlinkedSymbol(t *testing.T) {
	ctx := context.Background()
	_, pool := tataSteelFixture(t)
	ids := symbolIDs(t, pool)
	_, err := entities.Retract(ctx, pool, ctrlISIN, "", "", false)
	require.ErrorContains(t, err, "nothing to undo")
	_, err = entities.Retract(ctx, pool, itoa(ids[ctrlISIN]), "", "", true)
	require.ErrorContains(t, err, "nothing to undo")
}

// chainISINs are three generations of one issuer, used for the cases that
// need an entity with more than one linked member. Two members is the shape
// every other test uses, and it is the shape that hides both defects below.
const (
	chainA = "INE777C01011"
	chainB = "INE777C01029"
	chainC = "INE777C01037"
)

// chainFixture registers three ISINs of one issuer with disjoint, ordered
// spans. Only a few bars each: what these tests exercise is the map, and the
// symbols registry is all `apply` reads.
func chainFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testutil.Pool(t)
	store := market.NewStore(pool)
	var bars []market.Bar
	for _, s := range []struct {
		isin, ticker string
		from         time.Time
	}{
		{chainA, "POCL1", market.Day(2015, 3, 2)},
		{chainB, "POCL2", market.Day(2019, 5, 27)},
		{chainC, "POCL3", market.Day(2022, 1, 3)},
	} {
		for i := 0; i < 5; i++ {
			bars = append(bars, bar(s.isin, s.ticker, s.from.AddDate(0, 0, i), 100, 5_000_000))
		}
	}
	_, err := store.InsertBars(context.Background(), market.SourceBhavcopy, bars)
	require.NoError(t, err)
	return pool
}

func chainRoster(t *testing.T, links ...entities.Link) (*entities.Roster, []byte) {
	t.Helper()
	r := &entities.Roster{Version: 1, Links: links}
	d, err := r.ComputeDigest()
	require.NoError(t, err)
	return r, d
}

func TestApplyRefusesToRenameAnEntityWhenAnOlderPredecessorAppears(t *testing.T) {
	// Design 4.1 names this case exactly: "if a predecessor OLDER than the
	// current root is later discovered (a pre-2011 backfill), it joins by
	// pointing at the existing entity_id rather than renaming the entity."
	// `plan` derives every row's entity from the roster's own chain root, so
	// a roster that gains an older predecessor renames the entity instead --
	// and the rename is then rejected by migration 0004's flatness trigger
	// with "re-point every member or none", which is advice design 4.1 and 10
	// both forbid taking.
	//
	// The growth path design 4.1 describes is not available: the row it wants
	// is (symbol P, entity A) with no predecessor to record, and design 4.2's
	// CHECK requires predecessor IS NOT NULL on every non-retraction row. So
	// this refuses, in words that say what happened, rather than improvising
	// a row shape the design does not define. See docs/DESIGN.md, Stage 2.
	ctx := context.Background()
	pool := chainFixture(t)

	first, d1 := chainRoster(t, chainLink(chainB, chainC, "02->03", "2022-01-02", "2022-01-03"))
	_, err := entities.Apply(ctx, pool, first, d1, "", false)
	require.NoError(t, err)

	grown, d2 := chainRoster(t,
		chainLink(chainA, chainB, "01->02", "2019-05-26", "2019-05-27"),
		chainLink(chainB, chainC, "02->03", "2022-01-02", "2022-01-03"))
	_, err = entities.Apply(ctx, pool, grown, d2, "", false)
	require.Error(t, err)
	require.ErrorContains(t, err, "is already the ENTITY of")
	require.ErrorContains(t, err, "renames the entity")
	require.NotContains(t, err.Error(), "re-point every member or none",
		"the trigger's own advice contradicts design 4.1; the operator must not be handed it")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*)::int FROM symbol_links`).Scan(&n))
	require.Equal(t, 1, n, "and nothing partial is written into a table with no DELETE")
}

func TestRetractSymbolRefusesAMemberOfAMultiMemberEntity(t *testing.T) {
	// Design 4.6 keeps `--symbol` "as an alias only for the single-member
	// case". Implementing only the literal half of that sentence -- refuse
	// when the named symbol is a ROOT with members -- lets a NON-root member
	// of a three-member chain through: its siblings stay pointed at the old
	// root, the CLI reports the entity dissolved, and one company is split
	// across two entity ids with a hole in the middle of the survivor's
	// history. Nothing downstream sees it: the map is still flat (I1 silent),
	// disjoint (I2 silent) and same-issuer (I3 silent).
	ctx := context.Background()
	pool := chainFixture(t)
	store := market.NewStore(pool)
	r, digest := chainRoster(t,
		chainLink(chainA, chainB, "01->02", "2019-05-26", "2019-05-27"),
		chainLink(chainB, chainC, "02->03", "2022-01-02", "2022-01-03"))
	res, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)
	require.Len(t, res.Rows, 2)
	ids := symbolIDs(t, pool)

	_, err = entities.Retract(ctx, pool, itoa(ids[chainB]), "", "", true)
	require.Error(t, err)
	require.ErrorContains(t, err, "split one company across two entity ids")
	require.ErrorContains(t, err, itoa(ids[chainC]), "the operator has to be told which siblings they would strand")
	require.ErrorContains(t, err, "--entity")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*)::int FROM symbol_links`).Scan(&n))
	require.Equal(t, 2, n, "a refusal writes nothing")

	// And the verb that does the job says what it did.
	out, err := entities.Retract(ctx, pool, itoa(ids[chainA]), "wrong", "", false)
	require.NoError(t, err)
	require.Len(t, out.Members, 2, "every member of the entity gets a row, in one transaction")
	require.Zero(t, out.Remaining)
	require.False(t, out.SymbolScoped)
	violations, err := store.CheckEntityInvariants(ctx, time.Now())
	require.NoError(t, err)
	require.Empty(t, violations)
}

func TestApplyRefusesACorrectedBoundaryRatherThanSkippingIt(t *testing.T) {
	// `boundary` is not provenance -- design 0 withdrew that claim -- it is
	// the only column that says which member of an entity is in force on a
	// date. Deciding "already linked" on entity_id alone therefore makes a
	// re-apply of a CORRECTED roster a silent no-op: the wrong boundary stays
	// in the store and the operator is told "skipped 1 already-linked".
	//
	// It is reachable from the design's own monthly ops flow: bars is
	// insert-only, but a backfill can still add EARLIER sessions (design 5.3
	// cites 19 previously-unknown weekend sessions added by one run), which
	// moves a successor's first bar and hence effective_from.
	ctx := context.Background()
	store, pool := tataSteelFixture(t)

	// A roster that puts the boundary one session too early. Nothing in
	// Validate can catch this: it re-reads the numbers the file states, and
	// the file states a first bar the archive does not have.
	wrong := tataSteel()
	wrong.Gates.PredecessorLastBar = "2022-07-27"
	wrong.Gates.SuccessorFirstBar = "2022-07-28"
	wrong.EffectiveFrom = "2022-07-28"
	wr, wd := chainRoster(t, wrong)
	_, err := entities.Apply(ctx, pool, wr, wd, "", false)
	require.NoError(t, err)

	// The damage, before the correction: the bar for 2022-07-28 physically
	// belongs to the predecessor and the entity labels it with the successor.
	bars, err := store.BarsForDate(ctx, market.SourceBhavcopy, market.Day(2022, 7, 28), dbNow(t, pool))
	require.NoError(t, err)
	var labelled string
	for _, b := range bars {
		if b.Ticker == "TATASTEEL" {
			labelled = b.ISIN
		}
	}
	require.Equal(t, succISIN, labelled, "the wrong boundary puts the wrong member in force on this session")

	// Re-applying the CORRECTED roster must not be reported as a no-op.
	right, rd := tataSteelRoster(t)
	res, err := entities.Apply(ctx, pool, right, rd, "", false)
	require.Error(t, err)
	require.Nil(t, res)
	require.ErrorContains(t, err, "2022-07-28", "the stored boundary")
	require.ErrorContains(t, err, "2022-07-29", "and the one the roster now says")
	require.ErrorContains(t, err, "retract --symbol")

	var stored time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT boundary FROM symbol_links ORDER BY ingested_at DESC LIMIT 1`).Scan(&stored))
	require.Equal(t, market.Day(2022, 7, 28), stored, "and it changed nothing while it refused")

	// The path the error names works, and the corrected answer is the one a
	// later pin gets.
	ids := symbolIDs(t, pool)
	_, err = entities.Retract(ctx, pool, itoa(ids[succISIN]), "boundary was wrong", "", true)
	require.NoError(t, err)
	_, err = entities.Apply(ctx, pool, right, rd, "", false)
	require.NoError(t, err)

	bars, err = store.BarsForDate(ctx, market.SourceBhavcopy, market.Day(2022, 7, 28), dbNow(t, pool))
	require.NoError(t, err)
	labelled = ""
	for _, b := range bars {
		if b.Ticker == "TATASTEEL" {
			labelled = b.ISIN
		}
	}
	require.Equal(t, predISIN, labelled, "the session is back under the member that actually holds it")
}

func TestApplySkipsAGenuineRepeatOfTheSameLine(t *testing.T) {
	// The other side of the comparison: a re-apply of the SAME roster is
	// still a skip, or every monthly run would double a table with no DELETE.
	ctx := context.Background()
	_, pool := tataSteelFixture(t)
	r, digest := tataSteelRoster(t)
	_, err := entities.Apply(ctx, pool, r, digest, "", false)
	require.NoError(t, err)

	// Even with a different roster_sha: the sha covers the whole file, so
	// treating a changed one as a change to every row would rewrite 444 rows
	// for an edit to one line. What is compared is what the row MEANS.
	same := tataSteel()
	same.Gates.PredecessorLastBar = "2022-07-28"
	same.Gates.SuccessorFirstBar = "2022-07-29"
	same.EffectiveFrom = "2022-07-29"
	same.Note = "reworded during review, which moves the digest and nothing else"
	r2, d2 := chainRoster(t, same)
	require.NotEqual(t, digest, d2)
	res, err := entities.Apply(ctx, pool, r2, d2, "", false)
	require.NoError(t, err)
	require.Empty(t, res.Rows)
	require.Len(t, res.Skipped, 1)
}

func TestApplyExtendsAChainAtTheEntityTheStoreAlreadyHolds(t *testing.T) {
	// Design 4.1's named growth case: "extending a chain (a fresh split in
	// 2027) is one INSERT pointing the new successor at the EXISTING
	// entity_id, with no re-pointing of anything."
	//
	// A roster does not have to carry the whole history to be applied --
	// `entities link` writes a one-line file, and a reviewer may prune a diff
	// to the pair that changed -- so the roster in hand can be B -> C alone.
	// Deriving the entity from the roster's own chain root then names B, and
	// B already resolves to A: one company across two entity ids. The
	// flatness trigger does stop it, but only with "B is the entity of other
	// symbols; re-point every member or none", which is the advice design 4.1
	// and 10 both forbid taking, over a roster that asked for nothing wrong.
	ctx := context.Background()
	pool := chainFixture(t)
	store := market.NewStore(pool)

	first, d1 := chainRoster(t, chainLink(chainA, chainB, "01->02", "2019-05-26", "2019-05-27"))
	_, err := entities.Apply(ctx, pool, first, d1, "", false)
	require.NoError(t, err)
	ids := symbolIDs(t, pool)

	// The 2027 split, as its own roster. Nothing in this file mentions A.
	grown, d2 := chainRoster(t, chainLink(chainB, chainC, "02->03", "2022-01-02", "2022-01-03"))
	res, err := entities.Apply(ctx, pool, grown, d2, "", false)
	require.NoError(t, err, "a fresh split is one INSERT, not a re-point of anything")
	require.Len(t, res.Rows, 1)
	require.Equal(t, ids[chainA], res.Rows[0].EntityID,
		"the entity is the one the store already holds, not the root this roster happens to walk to")
	require.Equal(t, chainA, res.Rows[0].EntityISIN,
		"and it is named by an ISIN this roster never mentions, so apply had to look it up")

	entityOf := func(isin string) int64 {
		t.Helper()
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT entity_id FROM symbol_links WHERE symbol_id = $1 ORDER BY ingested_at DESC LIMIT 1`,
			ids[isin]).Scan(&id))
		return id
	}
	require.Equal(t, ids[chainA], entityOf(chainB))
	require.Equal(t, ids[chainA], entityOf(chainC), "one company, one entity id")
	violations, err := store.CheckEntityInvariants(ctx, time.Now())
	require.NoError(t, err)
	require.Empty(t, violations)

	// Re-applying the same one-line roster is still a skip, because the row
	// it plans is the row that is already there.
	again, err := entities.Apply(ctx, pool, grown, d2, "", false)
	require.NoError(t, err)
	require.Empty(t, again.Rows)
	require.Len(t, again.Skipped, 1)

	// And a retracted root falls back to the roster's answer, which is
	// correct: a retraction row sets entity_id = symbol_id, so B is its own
	// entity again and a B -> C roster roots at B.
	_, err = entities.Retract(ctx, pool, itoa(ids[chainA]), "two different companies", "", false)
	require.NoError(t, err)
	res, err = entities.Apply(ctx, pool, grown, d2, "", false)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	require.Equal(t, ids[chainB], res.Rows[0].EntityID,
		"B resolves to itself after the retraction, so it is the entity again")
}
