// Package bhavcopy downloads and parses NSE's daily equity bhavcopy archives,
// the unadjusted end-of-day record that still contains delisted names.
package bhavcopy

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// Parse reads one bhavcopy CSV in either the legacy (cmDDMMMYYYYbhav.csv) or
// the UDiFF (BhavCopy_NSE_CM_*.csv) layout, chosen by the header, and keeps
// only series EQ rows. name is used in error messages only.
func Parse(name string, r io.Reader) ([]market.Bar, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("bhavcopy %s: header: %w", name, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	switch {
	case has(col, "TradDt", "TckrSymb", "SctySrs"):
		return parseRows(name, cr, udiffLayout, col)
	case has(col, "SYMBOL", "SERIES", "TIMESTAMP"):
		return parseRows(name, cr, legacyLayout, col)
	default:
		return nil, fmt.Errorf("bhavcopy %s: unrecognised header %v", name, header)
	}
}

// ParseLegacy and ParseUDiFF exist for callers that already know the layout.
func ParseLegacy(r io.Reader) ([]market.Bar, error) { return Parse("legacy", r) }
func ParseUDiFF(r io.Reader) ([]market.Bar, error)  { return Parse("udiff", r) }

func has(col map[string]int, names ...string) bool {
	for _, n := range names {
		if _, ok := col[n]; !ok {
			return false
		}
	}
	return true
}

// layout maps the fields we need onto a format's column names. dateFmts is
// tried in order; the first layout that parses the column value wins.
type layout struct {
	date, isin, ticker, series, open, high, low, close, volume, turnover string
	dateFmts                                                             []string
}

// legacyLayout's TIMESTAMP column is almost always four-digit DD-Mon-YYYY,
// but NSE's 2020-07-13 archive uses a two-digit year (DD-Mon-YY) throughout
// that one session. Both forms are accepted; four-digit is tried first since
// it is the overwhelming majority.
var legacyLayout = layout{date: "TIMESTAMP", dateFmts: []string{"02-Jan-2006", "02-Jan-06"}, isin: "ISIN", ticker: "SYMBOL", series: "SERIES",
	open: "OPEN", high: "HIGH", low: "LOW", close: "CLOSE", volume: "TOTTRDQTY", turnover: "TOTTRDVAL"}

var udiffLayout = layout{date: "TradDt", dateFmts: []string{"2006-01-02"}, isin: "ISIN", ticker: "TckrSymb", series: "SctySrs",
	open: "OpnPric", high: "HghPric", low: "LwPric", close: "ClsPric", volume: "TtlTradgVol", turnover: "TtlTrfVal"}

func parseRows(name string, cr *csv.Reader, l layout, col map[string]int) ([]market.Bar, error) {
	for _, need := range []string{l.date, l.isin, l.ticker, l.series, l.open, l.high, l.low, l.close, l.volume, l.turnover} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("bhavcopy %s: missing column %q", name, need)
		}
	}
	field := func(rec []string, c string) string {
		i := col[c]
		if i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var bars []market.Bar
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bhavcopy %s line %d: %w", name, line, err)
		}
		if field(rec, l.series) != "EQ" {
			continue
		}
		dateVal := field(rec, l.date)
		var d time.Time
		for _, fm := range l.dateFmts {
			d, err = time.Parse(fm, dateVal)
			if err == nil {
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("bhavcopy %s line %d: date %q: %w", name, line, dateVal, err)
		}
		b := market.Bar{ISIN: field(rec, l.isin), Ticker: field(rec, l.ticker), Series: "EQ",
			Date: market.Day(d.Year(), d.Month(), d.Day())}
		nums := [6]float64{}
		for i, c := range []string{l.open, l.high, l.low, l.close, l.volume, l.turnover} {
			v, err := strconv.ParseFloat(field(rec, c), 64)
			if err != nil {
				return nil, fmt.Errorf("bhavcopy %s line %d: %s %q: %w", name, line, c, field(rec, c), err)
			}
			// ParseFloat accepts "nan"/"inf" with a nil error; see market.Finite.
			if err := market.Finite(c, v); err != nil {
				return nil, fmt.Errorf("bhavcopy %s line %d: %w", name, line, err)
			}
			nums[i] = v
		}
		b.Open, b.High, b.Low, b.Close = nums[0], nums[1], nums[2], nums[3]
		vol, err := market.Count(l.volume, nums[4])
		if err != nil {
			return nil, fmt.Errorf("bhavcopy %s line %d: %w", name, line, err)
		}
		b.Volume = vol
		turnover := nums[5]
		b.Turnover = &turnover
		if b.ISIN == "" {
			return nil, fmt.Errorf("bhavcopy %s line %d: %s has no ISIN", name, line, b.Ticker)
		}
		bars = append(bars, b)
	}
	return bars, nil
}

// udiffStart is the first session NSE published in the UDiFF layout.
var udiffStart = market.Day(2024, 7, 8)

// URLFor returns the archive URL for a session date in the layout NSE used then.
func URLFor(d time.Time) string {
	if d.Before(udiffStart) {
		mon := strings.ToUpper(d.Month().String()[:3])
		return fmt.Sprintf("https://nsearchives.nseindia.com/content/historical/EQUITIES/%d/%s/cm%02d%s%dbhav.csv.zip",
			d.Year(), mon, d.Day(), mon, d.Year())
	}
	return fmt.Sprintf("https://nsearchives.nseindia.com/content/cm/BhavCopy_NSE_CM_0_0_0_%s_F_0000.csv.zip",
		d.Format("20060102"))
}
