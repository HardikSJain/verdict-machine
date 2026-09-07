package entities_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/entities"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// Design 8.14: the equivalence re-run, WITH links. It is the gate on this
// stage.
//
// Design 6's "4,100 of 4,100 symbols agree" check was run with symbol_links
// EMPTY, where every entity is a singleton and the entity path collapses to
// the per-symbol path by construction. It cannot exercise the multi-member
// path, which is the entire change -- and the rule it certified was wrong on
// 324 of 520 live entities. This test is that check run in the configuration
// that matters: the real roster, applied by the real writer, asked over a
// grid of dates spanning the archive.
//
// Two things make it bite rather than pass by construction.
//
// The MAP is real. It is internal/market/entities/roster.json exactly as
// generated against the live 13.8M-bar store: 444 links, 415 entities, 29 of
// them three members long, with every boundary date the archive actually
// holds. A hand-built fixture would have picked convenient dates; this one
// has the dates NSE picked.
//
// The symbols rows are written by DIRECT INSERT at valid_from = '0001-01-01',
// with each successor registered BEFORE its predecessor. That is the
// production state -- 5,172 of 5,816 live symbols rows carry migration 0002's
// sentinel, and 3,745 of 4,100 symbol_ids do -- and it is unreproducible
// through EnsureSymbols, which rejects a zero date by design. It is also the
// exact shape that made revision 1's label rule resolve TATASTEEL, ICICIBANK,
// SBIN and AXISBANK to dead ISINs: with valid_from tied for every member on
// every date, the winner was decided by which member the backfill happened to
// register last.
//
// The bars are synthetic, and that is the honest limit of this test: it
// proves the member-interval RULE holds over a real map and a real calendar,
// not that the live store's 13.8M rows are labelled correctly. Only running
// it against the live store after `apply` would prove that, and applying to
// the live store is the human's decision.

// gridDates spans the archive, and is the grid the design measured revision
// 1's rule on: it scored 86/291, 56/314, 111/315, 195/393 and 160/423.
var gridDates = []time.Time{
	market.Day(2013, 1, 2),
	market.Day(2015, 1, 2),
	market.Day(2018, 1, 2),
	market.Day(2022, 10, 31),
	market.Day(2026, 9, 4),
}

func TestEntityLabelPicksTheMemberHoldingTheBar(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Pool(t)
	store := market.NewStore(pool)

	roster, digest, err := entities.Load("roster.json")
	require.NoError(t, err, "the committed roster must pass its own gates every time it is read")
	require.NotEmpty(t, roster.Links)

	// Member intervals, read off the roster's own recorded dates: a member is
	// in force from its first bar until its successor's first bar.
	type member struct {
		isin        string
		from, until time.Time // until is exclusive; zero means open-ended
	}
	successorOf := map[string]entities.Link{}
	predecessorOf := map[string]entities.Link{}
	for _, l := range roster.Links {
		successorOf[l.Predecessor] = l
		predecessorOf[l.Successor] = l
	}
	members := map[string]*member{}
	get := func(isin string) *member {
		if m, ok := members[isin]; ok {
			return m
		}
		m := &member{isin: isin}
		members[isin] = m
		return m
	}
	for _, l := range roster.Links {
		first, err := time.Parse("2006-01-02", l.Gates.SuccessorFirstBar)
		require.NoError(t, err)
		last, err := time.Parse("2006-01-02", l.Gates.PredecessorLastBar)
		require.NoError(t, err)
		get(l.Successor).from = first
		get(l.Predecessor).until = first
		if p := get(l.Predecessor); p.from.IsZero() {
			// A chain root's own first bar is not in the roster; the archive
			// starts in 2011, so anything before the grid will do.
			p.from = market.Day(2011, 9, 2)
		}
		_ = last
	}

	// Register every member by direct INSERT, successors first, at the
	// sentinel valid_from. See the comment above: this is the production
	// state and EnsureSymbols cannot produce it.
	names := make([]string, 0, len(members))
	for isin := range members {
		names = append(names, isin)
	}
	sort.Slice(names, func(i, j int) bool {
		// Successors before predecessors, so ingested_at order is the
		// opposite of chain order for every entity.
		a, b := names[i], names[j]
		if _, ok := predecessorOf[a]; ok != func() bool { _, ok := predecessorOf[b]; return ok }() {
			return ok
		}
		return a < b
	})
	ids := map[string]int64{}
	for _, isin := range names {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO symbols (isin, ticker, valid_from, ingested_at)
			VALUES ($1, $2, DATE '0001-01-01', now())
			RETURNING symbol_id`, isin, tickerFor(roster, isin)).Scan(&id))
		ids[isin] = id
	}

	// One bar per member per grid date inside that member's own interval.
	expect := map[time.Time]map[int64]string{} // date -> symbol_id -> isin
	for _, d := range gridDates {
		expect[d] = map[int64]string{}
		for isin, m := range members {
			if d.Before(m.from) || (!m.until.IsZero() && !d.Before(m.until)) {
				continue
			}
			insertBar(t, pool, ids[isin], d)
			expect[d][ids[isin]] = isin
		}
	}

	res, err := entities.Apply(ctx, pool, roster, digest, "", false)
	require.NoError(t, err)
	require.Len(t, res.Rows, len(roster.Links), "every roster line becomes exactly one row")

	// The invariants must hold over the whole seeded map before its labels
	// are worth checking.
	violations, err := store.CheckEntityInvariants(ctx, dbNow(t, pool))
	require.NoError(t, err)
	require.Empty(t, violations,
		"444 links over 415 entities must be flat, disjoint within a source, and issuer-consistent")

	// The check itself: on every grid date, every bar must come back labelled
	// with the ISIN of the member that physically holds it.
	total, multi := 0, 0
	for _, d := range gridDates {
		bars, err := store.BarsForDate(ctx, market.SourceBhavcopy, d, dbNow(t, pool))
		require.NoErrorf(t, err, "%s", d.Format("2006-01-02"))
		require.Equal(t, len(expect[d]), len(bars), "every bar written for %s comes back", d.Format("2006-01-02"))
		onDate := 0
		for _, b := range bars {
			want := expect[d][b.SymbolID]
			require.NotEmpty(t, want, "an unexpected bar for symbol %d on %s", b.SymbolID, d.Format("2006-01-02"))
			require.Equalf(t, want, b.ISIN,
				"on %s the bar is physically %s (symbol %d) but the entity is labelled %s: the label rule picked a member that does not hold this session",
				d.Format("2006-01-02"), want, b.SymbolID, b.ISIN)
			if b.EntityID != b.SymbolID {
				onDate++
			}
			total++
		}
		multi += onDate
		t.Logf("%s: %d bars, %d of them under a non-root member, all labelled with the member that holds them",
			d.Format("2006-01-02"), len(bars), onDate)
	}
	require.Greater(t, total, 1500, "the grid must actually exercise the map")
	require.Greater(t, multi, 300,
		"and it must exercise the MULTI-MEMBER path: a grid where every bar sits on a chain root would pass by construction, which is exactly what design 6's 4,100-of-4,100 check did")
}

func tickerFor(r *entities.Roster, isin string) string {
	for _, l := range r.Links {
		if l.Successor == isin || l.Predecessor == isin {
			if l.TickerAtBoundary != "" {
				return l.TickerAtBoundary
			}
			return isin
		}
	}
	return isin
}

// insertBar writes one EQ bar under a symbol_id without going through
// InsertBars, which would register the ISIN and so write the very symbols row
// the sentinel fixture exists to avoid.
func insertBar(t *testing.T, pool *pgxpool.Pool, symbolID int64, d time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO bars (symbol_id, date, source, series, open, high, low, close, volume, turnover, content_hash)
		VALUES ($1, $2, 'nse-bhavcopy', 'EQ', 100, 100, 100, 100, 1000, 100000, $3)`,
		symbolID, d, []byte(fmt.Sprintf("%d|%s", symbolID, d.Format("2006-01-02"))))
	require.NoError(t, err)
}
