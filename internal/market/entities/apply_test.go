package entities_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market/entities"
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
