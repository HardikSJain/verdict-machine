package market_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// detected finds the action this detector derived for one entity on one date,
// or reports that it derived none.
func detected(t *testing.T, adjs []market.Adjustment, entity int64, on time.Time) (market.Adjustment, bool) {
	t.Helper()
	for _, a := range adjs {
		if a.EntityID == entity && a.ExDate.Equal(on) {
			return a, true
		}
	}
	return market.Adjustment{}, false
}

// A corporate action is derived by comparing the unadjusted archive against the
// adjusted one: the quotient of the two closes is the cumulative adjustment
// ahead of that session, and it STEPS at every action. The whole method turns
// on the two series meeting, and they are stored under different symbol rows,
// so the join key is the entire question.
//
// These three tests exist because that key has now been wrong twice, in
// opposite directions, and neither failure was caught by anything except a
// backtest reporting a held position falling by half with no explanation.

// TestDetectAdjustments_SpansAnISINReissueWhenTheCompanyAlsoRenamed is the Ami
// Organics case, and it is the one that a ticker join cannot see.
//
// NSE reissues the ISIN at a face-value split, so the unadjusted history is cut
// in two. eod2 files the whole history under the CURRENT ISIN. If the company
// then renames -- Ami Organics became Acutaas Chemicals -- the old ISIN's bars
// are labelled with the old ticker and the adjusted series with the new one, and
// a join on ticker never brings them together. The split is invisible, and a
// holder keeps the same share count at half the price.
//
// The entity link is what spans it here, which is why both keys are needed.
func TestDetectAdjustments_SpansAnISINReissueWhenTheCompanyAlsoRenamed(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Pool(t)
	store := market.NewStore(pool)

	const oldISIN, newISIN = "INE00FF01017", "INE00FF01025"
	before, ex := market.Day(2025, 4, 24), market.Day(2025, 4, 25)

	// Unadjusted: the old ISIN up to the split, the new one from it. The price
	// halves because the shares doubled.
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{
		barAt(oldISIN, "AMIORG", before, 2202.50),
		barAt(newISIN, "ACUTAAS", ex, 1069.80),
	})
	require.NoError(t, err)

	// Adjusted: one continuous series, filed under the CURRENT ISIN and the
	// CURRENT name, with the pre-split price already halved.
	_, err = store.InsertBars(ctx, market.SourceEod2, []market.Bar{
		barAt(newISIN, "ACUTAAS", before, 1101.25),
		barAt(newISIN, "ACUTAAS", ex, 1069.80),
	})
	require.NoError(t, err)

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: oldISIN, Ticker: "AMIORG", Date: before},
		{ISIN: newISIN, Ticker: "ACUTAAS", Date: ex},
	})
	require.NoError(t, err)
	old, current := ids[oldISIN], ids[newISIN]
	linkRow(t, pool, current, old, ex)

	adjs, sum, err := store.DetectAdjustments(ctx, 0.02, market.MinShareActionStep, time.Now())
	require.NoError(t, err)
	require.Positive(t, sum.Steps, "the two series must meet at all")

	got, ok := detected(t, adjs, old, ex)
	require.True(t, ok, "the split is only visible across the ISIN reissue, and the ticker changed at it")
	require.InDelta(t, 2.0, got.Ratio, 1e-9, "a 1:1 bonus doubles the share count")
}

// TestDetectAdjustments_SpansAnUnlinkedISINReissueWhenTheTickerHeld is the Yes
// Bank case, and it is the one an entity join cannot see.
//
// The roster has not linked these two ISINs -- Yes Bank holds three across two
// entities because one was quarantined -- so keyed by entity the factor series
// is cut in two at exactly the session the split lands on. The ticker is
// unchanged, and spans it.
func TestDetectAdjustments_SpansAnUnlinkedISINReissueWhenTheTickerHeld(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	const oldISIN, newISIN = "INE528G01019", "INE528G01035"
	before, ex := market.Day(2017, 9, 21), market.Day(2017, 9, 22)

	_, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{
		barAt(oldISIN, "YESBANK", before, 1850.00),
		barAt(newISIN, "YESBANK", ex, 370.00),
	})
	require.NoError(t, err)
	_, err = store.InsertBars(ctx, market.SourceEod2, []market.Bar{
		barAt(newISIN, "YESBANK", before, 370.00),
		barAt(newISIN, "YESBANK", ex, 370.00),
	})
	require.NoError(t, err)

	ids, err := store.EnsureSymbols(ctx, []market.Bar{
		{ISIN: oldISIN, Ticker: "YESBANK", Date: before},
		{ISIN: newISIN, Ticker: "YESBANK", Date: ex},
	})
	require.NoError(t, err)
	// Deliberately NOT linked: this is the quarantined-succession case.

	adjs, _, err := store.DetectAdjustments(ctx, 0.02, market.MinShareActionStep, time.Now())
	require.NoError(t, err)

	got, ok := detected(t, adjs, ids[newISIN], ex)
	require.True(t, ok, "no entity spans these two ISINs; only the ticker does")
	require.InDelta(t, 5.0, got.Ratio, 1e-9, "a 1:5 split multiplies the share count by five")
}

// TestDetectAdjustments_DiscardsAStepThePriceDoesNotConfirm is the guard that
// makes unioning two join keys safe.
//
// Widening the key widens the candidate set, and a wider candidate set is only
// safe because every candidate still has to be corroborated: eod2 adjusts for
// dividends as well as share actions, and a ticker can be REUSED by a different
// company years later. Both produce a quotient step. Neither produces the price
// fall that a genuine share action produces on the same session, so both are
// dropped here rather than multiplying a share count for free.
func TestDetectAdjustments_DiscardsAStepThePriceDoesNotConfirm(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))

	const isin = "INE123X01019"
	before, ex := market.Day(2019, 3, 11), market.Day(2019, 3, 12)

	// The quotient steps by 2 -- but the unadjusted price ROSE, so whatever
	// happened, it was not a 1:1 bonus.
	_, err := store.InsertBars(ctx, market.SourceBhavcopy, []market.Bar{
		barAt(isin, "KRITIKA", before, 200.00),
		barAt(isin, "KRITIKA", ex, 203.00),
	})
	require.NoError(t, err)
	_, err = store.InsertBars(ctx, market.SourceEod2, []market.Bar{
		barAt(isin, "KRITIKA", before, 100.00),
		barAt(isin, "KRITIKA", ex, 203.00),
	})
	require.NoError(t, err)

	ids, err := store.EnsureSymbols(ctx, []market.Bar{{ISIN: isin, Ticker: "KRITIKA", Date: ex}})
	require.NoError(t, err)

	adjs, sum, err := store.DetectAdjustments(ctx, 0.02, market.MinShareActionStep, time.Now())
	require.NoError(t, err)
	require.Positive(t, sum.Steps, "the step is there to be seen")
	require.Equal(t, 1, sum.Uncorroborated, "and it must be seen and then refused, not missed")

	_, ok := detected(t, adjs, ids[isin], ex)
	require.False(t, ok, "multiplying a share count with no matching price fall invents money")
}
