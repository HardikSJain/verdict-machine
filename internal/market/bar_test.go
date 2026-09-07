package market

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentHashChangesWithClose(t *testing.T) {
	a := Bar{ISIN: "INE081A01020", Ticker: "TATASTEEL", Series: "EQ", Date: Day(2022, 7, 28),
		Open: 98.1, High: 102, Low: 97.15, Close: 100.35, Volume: 137156107}
	b := a
	b.Close = 100.36
	require.Equal(t, a.ContentHash(), a.ContentHash(), "hash must be deterministic")
	require.NotEqual(t, a.ContentHash(), b.ContentHash())
	require.Len(t, a.ContentHash(), 32)
}

func TestDayIsUTCMidnight(t *testing.T) {
	d := Day(2015, 6, 30)
	require.Equal(t, "2015-06-30T00:00:00Z", d.Format("2006-01-02T15:04:05Z07:00"))
}
