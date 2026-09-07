package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// The fence, printed.
//
// UniverseMember.LastBreak is the only warning a human reading a universe
// gets that a name's price series snaps in half somewhere inside the window.
// After the entity change the series is continuous in IDENTITY -- one row,
// one ticker, 125 sessions -- and discontinuous in LEVEL, because
// nse-bhavcopy is unadjusted and the adjustments layer does not exist. A
// 12-1 momentum reading across a 1:10 split comes back -90% and looks like a
// number. Design 10 calls that the biggest risk in the design and names
// EntityBoundaries and LastBreak as the mitigation, "and they work only if
// M1's engine actually calls them"; a field nothing prints is a field nobody
// knows to call.

func universeFixture(lastBreak *time.Time) market.Universe {
	return market.Universe{
		Sessions:            125,
		RequiredLastSession: true,
		Members: []market.UniverseMember{
			{EntityID: 326, SymbolID: 3312, ISIN: "INE081A01020", Ticker: "TATASTEEL",
				MedianTurnover: 4.2e9, DaysPresent: 125, Fragments: 2, LastBreak: lastBreak},
			{EntityID: 12, SymbolID: 12, ISIN: "INE002A01018", Ticker: "RELIANCE",
				MedianTurnover: 9.9e9, DaysPresent: 125, Fragments: 1},
		},
	}
}

func TestWriteUniverse_PrintsLastBreakAndTheSplitWarningForAMemberThatHasOne(t *testing.T) {
	boundary := time.Date(2022, 7, 29, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	writeUniverse(&out, universeFixture(&boundary), time.Date(2022, 10, 31, 0, 0, 0, 0, time.UTC),
		market.SourceBhavcopy, 125)
	got := out.String()

	require.Contains(t, got, "last_break", "the column has to be in the header or the dates below it are unlabelled")
	require.Contains(t, got, "2022-07-29", "the boundary date itself, on the member that carries it")
	require.Contains(t, got, "1 of 2 members carry a succession boundary",
		"the count is what an operator scans for; a date buried in one row of 500 is not a warning")
	require.Contains(t, got, "wrong by the split factor",
		"the count alone says nothing about why it matters")
	require.Contains(t, got, "EntityBoundaries",
		"and the reader has to be told what to call before differencing across one")
	// The member with no boundary must not be blank: a blank cell in a
	// tab-separated column reads as a parse error, not as "no boundary".
	require.Contains(t, got, "RELIANCE\tINE002A01018\t9900000000\t125\t-\n")
}

func TestWriteUniverse_CountsZeroAndStaysQuietWhenNoMemberHasABoundary(t *testing.T) {
	var out bytes.Buffer
	writeUniverse(&out, universeFixture(nil), time.Date(2022, 10, 31, 0, 0, 0, 0, time.UTC),
		market.SourceBhavcopy, 125)
	got := out.String()

	// Printed even at zero, and this is deliberate. The live store's map is
	// empty until a human applies the roster, so "0 of N" is the operator's
	// evidence that the map was consulted and came back empty -- which is a
	// different statement from the fence not being wired up at all.
	require.Contains(t, got, "0 of 2 members carry a succession boundary")
	require.NotContains(t, got, "wrong by the split factor",
		"the warning is about boundaries in this window; with none, it is noise that trains an operator to skip the header")
	require.Contains(t, got, "TATASTEEL\tINE081A01020\t4200000000\t125\t-\n")
}
