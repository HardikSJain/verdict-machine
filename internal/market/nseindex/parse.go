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
	"sort"
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

// dateFmts are the layouts the archive's Index Date column has actually used,
// and the last of them is why this is a list rather than a constant.
//
// Hyphenated day-first is the norm. A backfill over 3,600 sessions found two
// departures, neither visible to hand-sampling:
//
//   - Slashes for stretches of 2014 and 2015 ("09/06/2014"), then back.
//   - **Month-first for a handful of April 2023 sessions**, inconsistently
//     inside the same week: the file at .../ind_close_all_06042023.csv carries
//     "04-06-2023" and the one at .../ind_close_all_12042023.csv carries
//     "12-04-2023". Month-first and day-first are indistinguishable whenever
//     both numbers are 12 or less, so this cannot be resolved by reading the
//     string.
//
// The filename is therefore the authority, and parseDate is handed the session
// it is supposed to be: a layout is accepted only if it reproduces that date.
// Without it, 2023-04-10's levels would have been filed under 2023-10-04, which
// is a real Wednesday session -- silent corruption rather than a loud failure.
// This is the same shape as M0's two-digit year in the 2020-07-13 bhavcopy: the
// archive is mostly consistent, and its exceptions surface only when something
// reads every file.
var dateFmts = []string{"02-01-2006", "02/01/2006", "01-02-2006", "01/02/2006"}

// parseDate decodes the Index Date column against the session the file was
// requested for.
//
// Trying layouts until one MATCHES rather than until one parses is the whole
// point. It cannot mask a genuinely wrong file: if the archive served another
// session entirely, no layout reproduces the requested date and the caller
// still errors.
func parseDate(raw string, want time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	var parsed []string
	for _, f := range dateFmts {
		d, err := time.Parse(f, raw)
		if err != nil {
			continue
		}
		got := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		if got.Equal(want) {
			return got, nil
		}
		parsed = append(parsed, got.Format(time.DateOnly))
	}
	if len(parsed) == 0 {
		return time.Time{}, fmt.Errorf("date %q matches none of %v", raw, dateFmts)
	}
	return time.Time{}, fmt.Errorf(
		"date %q reads as %s under every layout tried, none of them the requested %s; the archive served the wrong session",
		raw, strings.Join(parsed, " or "), want.Format(time.DateOnly))
}

// Parse reads one session's index CSV.
//
// wantDate is checked against every row's own Index Date column. The archive
// has always agreed with its filename in every file sampled, and this is what
// keeps that a fact rather than an assumption: NSE serving yesterday's file
// under today's URL would otherwise write yesterday's levels under today's date
// and be invisible until a trend rule acted on it.
func Parse(r io.Reader, wantDate time.Time) ([]market.IndexLevel, []string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(wantHeader)
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("index csv: reading header: %w", err)
	}
	if len(header) != len(wantHeader) {
		return nil, nil, fmt.Errorf("index csv: header has %d columns, want %d: %v",
			len(header), len(wantHeader), header)
	}
	for i, want := range wantHeader {
		if strings.TrimSpace(header[i]) != want {
			return nil, nil, fmt.Errorf("index csv: column %d is %q, want %q; the archive's layout changed and a silent reorder would misfile every value",
				i, strings.TrimSpace(header[i]), want)
		}
	}

	records, err := cr.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("index csv: %w", err)
	}

	// Count names before building anything. NSE publishes a genuine duplicate
	// on at least one session: on 2013-02-08 two DIFFERENT indices are both
	// labelled "CNX Alpha Index", closing at 4713.18 and 1572.90. One of them
	// is mislabelled at source and there is no way to tell which from inside
	// the file, so both are excluded and the collision is reported. Taking the
	// first would pick between them by row order, which is a coin flip written
	// as a fact; failing the whole session would throw away the other
	// ninety-odd indices over one bad row.
	count := map[string]int{}
	for _, rec := range records {
		if n := strings.TrimSpace(rec[0]); n != "" {
			count[n]++
		}
	}
	var anomalies []string
	for name, n := range count {
		if n > 1 {
			anomalies = append(anomalies, fmt.Sprintf(
				"%q appears %d times on %s; all copies excluded because the archive gives no way to choose between them",
				name, n, wantDate.Format(time.DateOnly)))
		}
	}
	sort.Strings(anomalies)

	var out []market.IndexLevel
	for _, rec := range records {
		name := strings.TrimSpace(rec[0])
		if name == "" || count[name] > 1 {
			continue
		}
		d, err := parseDate(rec[1], wantDate)
		if err != nil {
			return nil, nil, fmt.Errorf("index csv: %s: %w", name, err)
		}
		close, err := num(rec[5])
		if err != nil {
			return nil, nil, fmt.Errorf("index csv: %s on %s: closing value %q: %w",
				name, d.Format(time.DateOnly), rec[5], err)
		}
		if close == nil {
			// Never observed in any sampled file; an index with no close is
			// not a level and silently dropping it would understate coverage.
			return nil, nil, fmt.Errorf("index csv: %s on %s has no closing value",
				name, d.Format(time.DateOnly))
		}

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
				return nil, nil, fmt.Errorf("index csv: %s on %s: %q: %w", name, d.Format(time.DateOnly), f.raw, err)
			}
			*f.dst = v
		}
		if v, err := num(rec[8]); err != nil {
			return nil, nil, fmt.Errorf("index csv: %s on %s: volume %q: %w", name, d.Format(time.DateOnly), rec[8], err)
		} else if v != nil {
			n := int64(*v)
			l.Volume = &n
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil, anomalies, fmt.Errorf("index csv: no usable rows for %s", wantDate.Format(time.DateOnly))
	}
	return out, anomalies, nil
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
