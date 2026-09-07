package market

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// labelRow is one symbol's resolved identity on one (date, ingest) pair.
type labelRow struct {
	entityID int64
	isin     string
	ticker   string
}

// TestEntityLabelMatchesSymbolLabelForUnlinkedSymbols is design §8.1.
//
// With symbol_links empty -- which is exactly how Stage 1 deploys -- every
// entity is a singleton, entity_map returns the identity, entity_member
// returns the symbol itself, and entity_label's lateral is the one already in
// store.go. This test pins that degeneracy across both point-in-time axes:
// the pre-history fallback, the same-session correction, and the production
// sentinel valid_from all have to come back byte-identical from the two
// paths, on every sampled date and both ingest pins.
//
// WHAT THIS TEST DOES NOT DO, stated because revision 1 leaned on exactly
// this check as though it did: it says nothing whatever about a multi-member
// entity. Run with an empty link table the two queries are identical BY
// CONSTRUCTION, so it could not have caught a label rule that was wrong on
// 324 of 520 live entities -- and it did not. It rules out a regression for
// unlinked symbols and nothing else. §8.13 is the Stage 1 test that covers
// the part that matters, and §8.14 is the one that settles it.
func TestEntityLabelMatchesSymbolLabelForUnlinkedSymbols(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Pool(t)
	s := NewStore(pool)

	const (
		renamed = "INE095I01015" // several valid_from versions, plus a correction
		plain   = "INE002A01018" // one version
		legacy  = "INE081A01012" // the production shape: sentinel valid_from
	)
	v2015 := Day(2015, 6, 30)
	v2016 := Day(2016, 6, 30)

	_, err := s.EnsureSymbols(ctx, []Bar{
		{ISIN: renamed, Ticker: "SONASTEER", Date: v2015},
		{ISIN: plain, Ticker: "RELIANCE", Date: v2015},
	})
	require.NoError(t, err)
	_, err = s.EnsureSymbols(ctx, []Bar{{ISIN: renamed, Ticker: "JTEKTINDIA", Date: v2016}})
	require.NoError(t, err)

	// 5,172 of 5,816 live symbols rows carry migration 0002's 0001-01-01
	// sentinel and the writer cannot produce one, so it goes in by hand.
	_, err = pool.Exec(ctx,
		`INSERT INTO symbols (isin, ticker, valid_from) VALUES ($1, 'TATASTEEL', DATE '0001-01-01')`, legacy)
	require.NoError(t, err)

	firstPin := time.Now()
	time.Sleep(10 * time.Millisecond)
	// A same-session correction to the 2015 name, written later: it ties the
	// earliest valid_from and breaks the tie on ingested_at DESC.
	_, err = s.EnsureSymbols(ctx, []Bar{{ISIN: renamed, Ticker: "SONACOMS", Date: v2015}})
	require.NoError(t, err)

	entityPath := func(date, pin time.Time) map[int64]labelRow {
		out := map[int64]labelRow{}
		rows, err := pool.Query(ctx, `WITH `+
			entityMapCTE("$2")+`, `+
			entityMemberCTE("$1", "$2")+`, `+
			entityLabelCTE("$1", "$2")+`
			SELECT el.symbol_id, el.entity_id, el.isin, el.ticker FROM entity_label el
			ORDER BY el.symbol_id`, date, pin)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id int64
			var r labelRow
			require.NoError(t, rows.Scan(&id, &r.entityID, &r.isin, &r.ticker))
			out[id] = r
		}
		require.NoError(t, rows.Err())
		return out
	}

	symbolPath := func(date, pin time.Time) map[int64]labelRow {
		out := map[int64]labelRow{}
		rows, err := pool.Query(ctx, `
			SELECT s.symbol_id, sym.isin, sym.ticker
			FROM (SELECT DISTINCT symbol_id FROM symbols WHERE ingested_at <= $2) s
			JOIN LATERAL `+symbolLabelLateral("s.symbol_id", "$1", "$2")+` sym ON true
			ORDER BY s.symbol_id`, date, pin)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id int64
			var r labelRow
			require.NoError(t, rows.Scan(&id, &r.isin, &r.ticker))
			r.entityID = id // an unlinked symbol is its own entity
			out[id] = r
		}
		require.NoError(t, rows.Err())
		return out
	}

	dates := []time.Time{
		Day(2014, 1, 2), // predates every recorded version: the invented answer
		v2015,
		Day(2015, 12, 31),
		v2016,
		Day(2026, 9, 4),
	}
	for _, pin := range []time.Time{firstPin, time.Now()} {
		want := symbolPath(dates[0], pin)
		require.NotEmpty(t, want, "an empty comparison would pass vacuously")
		for _, d := range dates {
			expected := symbolPath(d, pin)
			require.Equal(t, expected, entityPath(d, pin),
				"unlinked, the entity path must be the per-symbol path character for character (date %s)",
				d.Format("2006-01-02"))
			for id, row := range expected {
				require.Equal(t, id, row.entityID, "a symbol with no link row is its own entity")
			}
		}
	}

	// The pins are actually different, so the loop above is two distinct
	// comparisons rather than the same one twice.
	require.NotEqual(t, symbolPath(v2015, firstPin), symbolPath(v2015, time.Now()),
		"the correction must be visible at the later pin and invisible at the earlier one")
}
