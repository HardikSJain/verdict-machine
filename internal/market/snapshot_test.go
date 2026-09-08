package market_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// TestSnapshotMovesWhenTheDataMovesAndNotOtherwise is the whole contract.
//
// A snapshot that never changed would certify nothing; one that changed on every
// call would make every replay look like a discrepancy. It must be a function of
// the visible rows and of nothing else -- not of the clock, not of the order
// rows were written in.
func TestSnapshotMovesWhenTheDataMovesAndNotOtherwise(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	empty, tables, err := store.SnapshotID(ctx, time.Now())
	require.NoError(t, err)
	require.Len(t, tables, 4, "bars, index_levels, symbols and symbol_links all count")

	_, err = store.InsertIndexLevels(ctx, market.SourceNSEIndex,
		[]market.IndexLevel{lvl("NIFTY50", "Nifty 50", market.Day(2020, 1, 1), 12000)})
	require.NoError(t, err)

	afterInsert, _, err := store.SnapshotID(ctx, time.Now())
	require.NoError(t, err)
	require.NotEqual(t, empty, afterInsert, "adding a row must move the snapshot")

	again, _, err := store.SnapshotID(ctx, time.Now())
	require.NoError(t, err)
	require.Equal(t, afterInsert, again, "and calling twice on unchanged data must not")

	// Pinned before the insert, the snapshot is the empty one again. This is
	// what makes a replay reproducible: the pin, not the wall clock, decides
	// what the run could see.
	past, _, err := store.SnapshotID(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, empty, past)
}

// TestRunsAreInsertOnlyAndDetectRepeats. A repeat of an identical run is not new
// evidence, and the only way to know is to have recorded the first.
func TestRunsAreInsertOnlyAndDetectRepeats(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	id, err := market.NewRunID()
	require.NoError(t, err)
	r := market.Run{
		RunID: id, Mode: "backtest", Strategy: "test", GitSHA: "abc",
		ConfigHash: "cfg", SnapshotID: "snap", IngestPin: time.Now(),
		PeriodFrom: market.Day(2013, 1, 1), PeriodTo: market.Day(2021, 12, 31),
		Result: map[string]any{"excess": -0.0573}, StartedAt: time.Now(), FinishedAt: time.Now(),
	}
	require.NoError(t, store.RecordRun(ctx, r))

	_, err = store.Pool().Exec(ctx, `UPDATE runs SET strategy = 'edited'`)
	require.Error(t, err, "a run row is a claim about a moment and cannot be rewritten")
	require.Contains(t, err.Error(), "insert-only")

	prior, err := store.PriorRuns(ctx, "cfg", "snap")
	require.NoError(t, err)
	require.Len(t, prior, 1)

	none, err := store.PriorRuns(ctx, "cfg", "different-snapshot")
	require.NoError(t, err)
	require.Empty(t, none, "a different snapshot is a different run, not a repeat")
}

// TestHoldoutRunsIsTheAudit. A sealed holdout is only sealed if somebody can
// tell whether it has been opened, and a document cannot answer that.
func TestHoldoutRunsIsTheAudit(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	holdout := market.Day(2022, 1, 1)

	clean, err := store.HoldoutRuns(ctx, holdout)
	require.NoError(t, err)
	require.Empty(t, clean)

	id, _ := market.NewRunID()
	require.NoError(t, store.RecordRun(ctx, market.Run{
		RunID: id, Mode: "backtest", Strategy: "in-sample only", GitSHA: "x",
		ConfigHash: "c", SnapshotID: "s", IngestPin: time.Now(),
		PeriodFrom: market.Day(2013, 1, 1), PeriodTo: market.Day(2021, 12, 31),
		StartedAt: time.Now(), FinishedAt: time.Now(),
	}))
	still, err := store.HoldoutRuns(ctx, holdout)
	require.NoError(t, err)
	require.Empty(t, still, "a run ending before the holdout has not opened it")

	id2, _ := market.NewRunID()
	require.NoError(t, store.RecordRun(ctx, market.Run{
		RunID: id2, Mode: "backtest", Strategy: "spent it", GitSHA: "x",
		ConfigHash: "c", SnapshotID: "s", IngestPin: time.Now(),
		PeriodFrom: market.Day(2022, 1, 1), PeriodTo: market.Day(2026, 9, 8),
		StartedAt: time.Now(), FinishedAt: time.Now(),
	}))
	spent, err := store.HoldoutRuns(ctx, holdout)
	require.NoError(t, err)
	require.Len(t, spent, 1)
	require.Equal(t, "spent it", spent[0].Strategy)
}
