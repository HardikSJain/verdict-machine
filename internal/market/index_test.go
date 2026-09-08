package market_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

func lvl(code, name string, d time.Time, close float64) market.IndexLevel {
	return market.IndexLevel{IndexCode: code, IndexName: name, Date: d, Close: close}
}

// TestIndexLevelsAreInsertOnly. The whole store's promise is that a past query
// reproduces, and a table that could be edited would break it silently for
// every run that read the edited row.
func TestIndexLevelsAreInsertOnly(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d := market.Day(2020, 1, 1)
	_, err := store.InsertIndexLevels(ctx, market.SourceNSEIndex,
		[]market.IndexLevel{lvl("NIFTY50", "Nifty 50", d, 12000)})
	require.NoError(t, err)

	_, err = store.Pool().Exec(ctx, `UPDATE index_levels SET close = 1`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "insert-only")

	_, err = store.Pool().Exec(ctx, `DELETE FROM index_levels`)
	require.Error(t, err)
}

// TestReingestingIdenticalLevelsIsANoOp. The backfill is resumable and gets
// re-run; without the content-hash skip every pass would append an identical
// version and make "how many times was this corrected" unanswerable.
func TestReingestingIdenticalLevelsIsANoOp(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d := market.Day(2020, 1, 1)
	batch := []market.IndexLevel{
		lvl("NIFTY50", "Nifty 50", d, 12182.5),
		lvl("NIFTYBANK", "Nifty Bank", d, 32000),
	}

	n, err := store.InsertIndexLevels(ctx, market.SourceNSEIndex, batch)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	n, err = store.InsertIndexLevels(ctx, market.SourceNSEIndex, batch)
	require.NoError(t, err)
	require.Zero(t, n, "identical content must write nothing")

	// A corrected close is a new version, not an overwrite.
	corrected := append([]market.IndexLevel(nil), batch...)
	corrected[0].Close = 12182.6
	n, err = store.InsertIndexLevels(ctx, market.SourceNSEIndex, corrected)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	var versions int
	require.NoError(t, store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM index_levels WHERE index_code='NIFTY50'`).Scan(&versions))
	require.Equal(t, 2, versions, "both versions are kept")
}

// TestIndexClosesIsPinned: a query pinned before a correction must still see
// what it saw. This is the same axis every other read in the store uses, and an
// index series that ignored it would make a replayed run's trend filter answer
// differently from the original.
func TestIndexClosesIsPinned(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d := market.Day(2020, 1, 1)

	_, err := store.InsertIndexLevels(ctx, market.SourceNSEIndex,
		[]market.IndexLevel{lvl("NIFTY50", "Nifty 50", d, 12000)})
	require.NoError(t, err)

	before := time.Now()
	time.Sleep(10 * time.Millisecond)
	_, err = store.InsertIndexLevels(ctx, market.SourceNSEIndex,
		[]market.IndexLevel{lvl("NIFTY50", "Nifty 50", d, 12500)})
	require.NoError(t, err)

	old, err := store.IndexCloses(ctx, "NIFTY50", market.Day(2019, 12, 1), market.Day(2020, 2, 1), before)
	require.NoError(t, err)
	require.Len(t, old, 1)
	require.Equal(t, 12000.0, old[0].Close, "a run pinned before the correction still sees 12000")

	now, err := store.IndexCloses(ctx, "NIFTY50", market.Day(2019, 12, 1), market.Day(2020, 2, 1), time.Now())
	require.NoError(t, err)
	require.Equal(t, 12500.0, now[0].Close)
}

// TestNameChangesFindsARebrandAndItsLevels is the evidence behind the alias
// table: a rebrand must leave the level alone, so a changeover whose close
// jumps is a different index wearing an old name.
func TestNameChangesFindsARebrandAndItsLevels(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	days := []time.Time{
		market.Day(2015, 11, 3), market.Day(2015, 11, 4),
		market.Day(2015, 11, 5), market.Day(2015, 11, 6),
	}
	names := []string{"CNX Nifty", "CNX Nifty", "Nifty 50", "Nifty 50"}
	closes := []float64{7955.0, 7960.0, 7965.0, 7970.0}
	for i, d := range days {
		_, err := store.InsertIndexLevels(ctx, market.SourceNSEIndex,
			[]market.IndexLevel{lvl("NIFTY50", names[i], d, closes[i])})
		require.NoError(t, err)
	}

	changes, err := store.NameChanges(ctx, "NIFTY50", time.Now())
	require.NoError(t, err)
	require.Len(t, changes, 1, "one rename inside the window")
	c := changes[0]
	require.Equal(t, "CNX Nifty", c.FromName)
	require.Equal(t, "Nifty 50", c.ToName)
	require.Equal(t, market.Day(2015, 11, 4), c.PrevDate)
	require.Equal(t, market.Day(2015, 11, 5), c.Date)
	require.InDelta(t, 7960.0, c.PrevClose, 1e-9)
	require.InDelta(t, 7965.0, c.Close, 1e-9)

	jump := c.Close/c.PrevClose - 1
	require.Less(t, jump, 0.06, "a rebrand leaves the level alone")
}

// TestInsertIndexLevelsRefusesADuplicateInOneBatch. Two rows for one index on
// one date would race each other into the same version key.
func TestInsertIndexLevelsRefusesADuplicateInOneBatch(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d := market.Day(2020, 1, 1)
	_, err := store.InsertIndexLevels(ctx, market.SourceNSEIndex, []market.IndexLevel{
		lvl("NIFTY50", "Nifty 50", d, 12000),
		lvl("NIFTY50", "Nifty 50", d, 12001),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "appears twice")
}

// TestIndexClosesRejectsAnInvertedRange, so a caller that swapped its endpoints
// gets an error rather than an empty series that looks like missing data.
func TestIndexClosesRejectsAnInvertedRange(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	_, err := store.IndexCloses(ctx, "NIFTY50", market.Day(2020, 2, 1), market.Day(2020, 1, 1), time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be before")
}
