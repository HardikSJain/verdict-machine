// Package market holds the daily bar model and the versioned bar store.
package market

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// Bar is one instrument's daily OHLCV as one source reported it. Prices are
// whatever the source says: eod2 bars are split/bonus adjusted in place,
// nse-bhavcopy bars are unadjusted.
type Bar struct {
	ISIN   string
	Ticker string
	Series string
	Date   time.Time // UTC midnight; see Day
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int64
	// Turnover is rupees traded that day. nil when the source does not report it.
	Turnover *float64
	// DeliveryQty is shares delivered that day. nil when the source does not report it.
	DeliveryQty *int64
}

// Day builds the UTC midnight time used for every calendar date in the system.
func Day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ContentHash is a deterministic sha256 over the fields that make a bar a bar.
// Two versions of the same (isin, date) with equal hashes are the same data.
func (b Bar) ContentHash() []byte {
	turnover := "-"
	if b.Turnover != nil {
		turnover = fmt.Sprintf("%.2f", *b.Turnover)
	}
	delivery := "-"
	if b.DeliveryQty != nil {
		delivery = fmt.Sprintf("%d", *b.DeliveryQty)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%.4f|%.4f|%.4f|%.4f|%d|%s|%s",
		b.ISIN, b.Date.Format("2006-01-02"), b.Series,
		b.Open, b.High, b.Low, b.Close, b.Volume, turnover, delivery)))
	return sum[:]
}
