package engine

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The frozen fixture.
//
// testdata/golden holds a real slice of the NSE archive -- 15 entities, 743
// sessions from 2018 through 2020, and the Nifty 500 over the same span -- and
// the backtest CI runs against it with no database at all.
//
// It is real data rather than a generator because a synthetic series has no
// splits, no renames, no holidays and no sessions where a name simply did not
// trade, and every one of those has already broken something in this repository.
// It is small because the point is not coverage; it is that a change which
// quietly alters a number fails the build instead of being noticed a year later
// in a result nobody can reproduce.

// LoadGolden reads the frozen fixture from a directory.
func LoadGolden(dir string) (*Fixture, []DatedClose, error) {
	bars, sessions, err := readGoldenBars(filepath.Join(dir, "bars.csv"))
	if err != nil {
		return nil, nil, err
	}
	index, err := readGoldenIndex(filepath.Join(dir, "index.csv"))
	if err != nil {
		return nil, nil, err
	}
	f := NewFixture(sessions)
	for date, byEntity := range bars {
		d, _ := time.Parse(time.DateOnly, date)
		d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		for _, b := range byEntity {
			f.AddBar(d, b)
		}
	}
	f.SetIndex("NIFTY500", index)
	return f, index, nil
}

func readGoldenBars(path string) (map[string][]Bar, []time.Time, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	r := csv.NewReader(fh)
	r.FieldsPerRecord = 7

	out := map[string][]Bar{}
	seen := map[string]bool{}
	var sessions []time.Time
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("golden bars: %w", err)
		}
		id, err := strconv.ParseInt(strings.TrimSpace(rec[0]), 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("golden bars: entity id %q: %w", rec[0], err)
		}
		date := strings.TrimSpace(rec[2])
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return nil, nil, fmt.Errorf("golden bars: date %q: %w", date, err)
		}
		var px [4]float64
		for i := range px {
			if px[i], err = strconv.ParseFloat(strings.TrimSpace(rec[3+i]), 64); err != nil {
				return nil, nil, fmt.Errorf("golden bars: %s %s: %w", rec[1], date, err)
			}
		}
		out[date] = append(out[date], Bar{
			EntityID: id, Scrip: strings.TrimSpace(rec[1]),
			Open: px[0], High: px[1], Low: px[2], Close: px[3],
		})
		if !seen[date] {
			seen[date] = true
			d, _ := time.Parse(time.DateOnly, date)
			sessions = append(sessions, time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC))
		}
	}
	if len(sessions) == 0 {
		return nil, nil, fmt.Errorf("golden bars: %s is empty", path)
	}
	return out, sessions, nil
}

func readGoldenIndex(path string) ([]DatedClose, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	r := csv.NewReader(fh)
	r.FieldsPerRecord = 2
	var out []DatedClose
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("golden index: %w", err)
		}
		d, err := time.Parse(time.DateOnly, strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, fmt.Errorf("golden index: date %q: %w", rec[0], err)
		}
		c, err := strconv.ParseFloat(strings.TrimSpace(rec[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("golden index: close %q: %w", rec[1], err)
		}
		out = append(out, DatedClose{
			Date:  time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC),
			Close: c,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("golden index: %s is empty", path)
	}
	return out, nil
}
