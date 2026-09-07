package market

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// goldenBar is the one fixed Bar the digests below are pinned against: the
// Tata Steel 2022-07-28 fixture row, with both optional fields populated.
func goldenBar() Bar {
	turnover := 215046652.2
	delivery := int64(52403544)
	return Bar{ISIN: "INE081A01020", Ticker: "TATASTEEL", Series: "EQ", Date: Day(2022, 7, 28),
		Open: 98.1, High: 102, Low: 97.15, Close: 100.35, Volume: 137156107,
		Turnover: &turnover, DeliveryQty: &delivery}
}

func TestContentHashChangesWithClose(t *testing.T) {
	a := Bar{ISIN: "INE081A01020", Ticker: "TATASTEEL", Series: "EQ", Date: Day(2022, 7, 28),
		Open: 98.1, High: 102, Low: 97.15, Close: 100.35, Volume: 137156107}
	b := a
	b.Close = 100.36
	require.Equal(t, a.ContentHash(), a.ContentHash(), "hash must be deterministic")
	require.NotEqual(t, a.ContentHash(), b.ContentHash())
	require.Len(t, a.ContentHash(), 32)
}

// TestContentHashIsGolden pins the exact digest of one fixed Bar, on both the
// populated and the nil paths of the two optional fields.
//
// ContentHash is the identity function the entire insert-only store is built
// on: InsertBars inserts a new version if and only if the hash differs. Any
// edit to the format string or the field set -- adding a field, changing
// %.4f, reordering -- makes every stored hash incomparable, so the next
// ingest inserts a brand-new version of every bar in the table. bars is
// insert-only, so those duplicates can never be removed, and every prior
// snapshot_id, the basis of the byte-for-byte replay claim, becomes
// unreproducible. Changing these two strings has to be a deliberate act.
func TestContentHashIsGolden(t *testing.T) {
	full := goldenBar()
	require.Equal(t, "4450355a8802226f155c2173275922ccecfb5beac4dca89fbcf083d2a51ed41d",
		hex.EncodeToString(full.ContentHash()))

	bare := full
	bare.Turnover = nil
	bare.DeliveryQty = nil
	require.Equal(t, "8874cd8349e2e8502ef508a41040e5aaa47ea22bfd6ea4d4fd6c753d14ed621c",
		hex.EncodeToString(bare.ContentHash()))
}

// TestContentHashCoversEveryInput varies each hashed field in turn. Only four
// of the ten were covered before: Open, High, Low, Volume, Series and
// DeliveryQty could each be dropped from the hash with the whole suite still
// green. That is the opposite of what the store promises -- a source that
// re-issues a file with a corrected high, or a reclassified series, or a
// corrected delivery quantity would be swallowed as "unchanged", permanently,
// with no error and no log line, and every downstream backtest would then run
// on data the store believes it corrected.
func TestContentHashCoversEveryInput(t *testing.T) {
	base := goldenBar()
	otherTurnover := *base.Turnover + 1
	otherDelivery := *base.DeliveryQty + 1

	for _, tc := range []struct {
		field  string
		mutate func(b *Bar)
	}{
		{"ISIN", func(b *Bar) { b.ISIN = "INE002A01018" }},
		{"Date", func(b *Bar) { b.Date = Day(2022, 7, 29) }},
		{"Series", func(b *Bar) { b.Series = "BE" }},
		{"Open", func(b *Bar) { b.Open = base.Open + 0.01 }},
		{"High", func(b *Bar) { b.High = base.High + 0.01 }},
		{"Low", func(b *Bar) { b.Low = base.Low + 0.01 }},
		{"Close", func(b *Bar) { b.Close = base.Close + 0.01 }},
		{"Volume", func(b *Bar) { b.Volume = base.Volume + 1 }},
		{"Turnover to another value", func(b *Bar) { b.Turnover = &otherTurnover }},
		{"Turnover to nil", func(b *Bar) { b.Turnover = nil }},
		{"DeliveryQty to another value", func(b *Bar) { b.DeliveryQty = &otherDelivery }},
		{"DeliveryQty to nil", func(b *Bar) { b.DeliveryQty = nil }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			got := base
			tc.mutate(&got)
			require.NotEqual(t, base.ContentHash(), got.ContentHash(),
				"a change to %s must be a new version, not a silently discarded correction", tc.field)
		})
	}

	// Ticker is deliberately not hashed: identity is the ISIN, the name lives
	// in symbols, and hashing it would make every rename re-version every bar
	// the symbol ever had.
	renamed := base
	renamed.Ticker = "TATASTL"
	require.Equal(t, base.ContentHash(), renamed.ContentHash(),
		"a ticker rename is a symbols version, not a bars version")
}

func TestDayIsUTCMidnight(t *testing.T) {
	d := Day(2015, 6, 30)
	require.Equal(t, "2015-06-30T00:00:00Z", d.Format("2006-01-02T15:04:05Z07:00"))
}
