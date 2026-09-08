// Package nseindex loads NSE's daily index archive.
//
// The archive is one CSV per session at
// nsearchives.nseindia.com/content/indices/ind_close_all_DDMMYYYY.csv, and it
// is far better behaved than the equity bhavcopy: one stable 13-column header
// from March 2012 to today, no quoted fields, one date format, and the closing
// value never missing. The gotchas are elsewhere -- a soft 404 that returns
// HTTP 200 with an HTML page, "-" for values an index does not publish, and a
// fourteen-year rebranding that renamed almost every index in the file.
package nseindex

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// Source is the ingest_log and index_levels source key.
const Source = market.SourceNSEIndex

// wantHeader is the archive's header, byte for byte, across every era sampled
// (2012-03 through 2026-09). It is asserted rather than tolerated: a silent
// column reorder would put turnover where volume belongs and nothing
// downstream would notice.
var wantHeader = []string{
	"Index Name", "Index Date", "Open Index Value", "High Index Value",
	"Low Index Value", "Closing Index Value", "Points Change", "Change(%)",
	"Volume", "Turnover (Rs. Cr.)", "P/E", "P/B", "Div Yield",
}

// Parse reads one session's index CSV.
//
// wantDate is checked against every row's own Index Date column. The archive
// has always agreed with its filename in every file sampled, and this is what
// keeps that a fact rather than an assumption: NSE serving yesterday's file
// under today's URL would otherwise write yesterday's levels under today's date
// and be invisible until a trend rule acted on it.
func Parse(r io.Reader, wantDate time.Time) ([]market.IndexLevel, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(wantHeader)
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("index csv: reading header: %w", err)
	}
	if len(header) != len(wantHeader) {
		return nil, fmt.Errorf("index csv: header has %d columns, want %d: %v",
			len(header), len(wantHeader), header)
	}
	for i, want := range wantHeader {
		if strings.TrimSpace(header[i]) != want {
			return nil, fmt.Errorf("index csv: column %d is %q, want %q; the archive's layout changed and a silent reorder would misfile every value",
				i, strings.TrimSpace(header[i]), want)
		}
	}

	var out []market.IndexLevel
	seen := map[string]bool{}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("index csv: %w", err)
		}
		name := strings.TrimSpace(rec[0])
		if name == "" {
			continue
		}
		d, err := time.Parse("02-01-2006", strings.TrimSpace(rec[1]))
		if err != nil {
			return nil, fmt.Errorf("index csv: %s: bad date %q: %w", name, rec[1], err)
		}
		d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		if !d.Equal(wantDate) {
			return nil, fmt.Errorf(
				"index csv: %s carries date %s in a file requested for %s; the archive served the wrong session",
				name, d.Format(time.DateOnly), wantDate.Format(time.DateOnly))
		}
		close, err := num(rec[5])
		if err != nil {
			return nil, fmt.Errorf("index csv: %s on %s: closing value %q: %w",
				name, d.Format(time.DateOnly), rec[5], err)
		}
		if close == nil {
			// Never observed in any sampled file; an index with no close is
			// not a level and silently dropping it would understate coverage.
			return nil, fmt.Errorf("index csv: %s on %s has no closing value",
				name, d.Format(time.DateOnly))
		}
		if seen[name] {
			return nil, fmt.Errorf("index csv: %s appears twice on %s", name, d.Format(time.DateOnly))
		}
		seen[name] = true

		l := market.IndexLevel{IndexCode: CodeFor(name, d), IndexName: name, Date: d, Close: *close}
		for _, f := range []struct {
			raw string
			dst **float64
		}{
			{rec[2], &l.Open}, {rec[3], &l.High}, {rec[4], &l.Low},
			{rec[6], &l.PointsChange}, {rec[7], &l.PctChange},
			{rec[9], &l.Turnover}, {rec[10], &l.PE}, {rec[11], &l.PB}, {rec[12], &l.DivYield},
		} {
			v, err := num(f.raw)
			if err != nil {
				return nil, fmt.Errorf("index csv: %s on %s: %q: %w", name, d.Format(time.DateOnly), f.raw, err)
			}
			*f.dst = v
		}
		if v, err := num(rec[8]); err != nil {
			return nil, fmt.Errorf("index csv: %s on %s: volume %q: %w", name, d.Format(time.DateOnly), rec[8], err)
		} else if v != nil {
			n := int64(*v)
			l.Volume = &n
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("index csv: no rows for %s", wantDate.Format(time.DateOnly))
	}
	return out, nil
}

// num parses one numeric cell.
//
// "-" is how the archive prints a value an index does not publish -- 27 of 165
// indices in a 2026 file print it for open, high and low because they are
// computed once a day rather than continuously -- and it is a null, not a zero.
// A zero would put those indices at the bottom of any ranking and make their
// "daily range" the whole of their level.
//
// Leading-dot floats (".1", "-.23") are how the archive writes small
// percentages, and Go's parser accepts them; commas inside numbers do not occur
// in any sampled file but are stripped anyway, because a thousands separator
// appearing once in fourteen years would otherwise fail an entire session.
func num(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return nil, nil
	}
	s = strings.ReplaceAll(s, ",", "")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}
