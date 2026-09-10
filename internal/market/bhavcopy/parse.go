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
	case has(col, "SYMBOL", "SERIES", "TIMESTAMP", "ISIN"):
		return parseRows(name, cr, legacyLayout, col)
	case has(col, "SYMBOL", "SERIES", "TIMESTAMP"):
		// Same header minus ISIN: NSE's archive before July 2011. Order
		// matters here -- the pre-ISIN header is a strict subset of the legacy
		// one, so the ISIN-bearing case has to be tested first or every modern
		// file would be read as an identity-less one.
		return parseRows(name, cr, preISINLayout, col)
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

// preISINLayout is NSE's archive before it began printing ISINs, which is
// every session from the exchange's first (1994-11-03) to some day between
// 1 June and 4 July 2011. Measured by probing: 2011-06-01 has no ISIN column
// and 2011-07-04 does.
//
// That column is why this project's own archive starts 2011-09-02. The start
// date was never written down as a decision and reads like an arbitrary flag
// default, but it is exactly where identity became available -- everything
// here keys on ISIN, so the era before it could not be stored at all. Seventeen
// years of the source of record sat behind one missing column.
//
// The DAY is not zero-padded in the older files -- "3-NOV-1994" against
// "03-JAN-2011" -- so the unpadded form is tried after the padded one. The
// month is upper case throughout and needs no special handling, because Go
// matches month names case-insensitively.
//
// isin is deliberately EMPTY rather than pointing at a column that is not
// there. Bars from this layout come back with no ISIN, which is the truthful
// reading of the file, and they cannot be stored until something assigns them
// an identity. market.Store rejects an empty ISIN outright so that state is
// loud rather than silent: without that guard every pre-ISIN ticker would
// collapse into a single symbol row keyed on the empty string.
var preISINLayout = layout{date: "TIMESTAMP", dateFmts: []string{"02-Jan-2006", "2-Jan-2006", "02-Jan-06"},
	isin: "", ticker: "SYMBOL", series: "SERIES",
	open: "OPEN", high: "HIGH", low: "LOW", close: "CLOSE", volume: "TOTTRDQTY", turnover: "TOTTRDVAL"}

var udiffLayout = layout{date: "TradDt", dateFmts: []string{"2006-01-02"}, isin: "ISIN", ticker: "TckrSymb", series: "SctySrs",
	open: "OpnPric", high: "HghPric", low: "LwPric", close: "ClsPric", volume: "TtlTradgVol", turnover: "TtlTrfVal"}

func parseRows(name string, cr *csv.Reader, l layout, col map[string]int) ([]market.Bar, error) {
	for _, need := range []string{l.date, l.isin, l.ticker, l.series, l.open, l.high, l.low, l.close, l.volume, l.turnover} {
		if need == "" {
			// The layout declares this field absent from the era, not missing
			// from the file. Only isin is ever empty; see preISINLayout.
			continue
		}
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
		b := market.Bar{Ticker: field(rec, l.ticker), Series: "EQ",
			Date: market.Day(d.Year(), d.Month(), d.Day())}
		if l.isin != "" {
			b.ISIN = field(rec, l.isin)
		}
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
		if l.isin != "" && b.ISIN == "" {
			// The file claims to carry ISINs and this row's is blank, which is
			// a defect in the row rather than a property of the era.
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
