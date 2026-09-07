// Package eod2 reads the CSV output of the eod2 project (GPL-3.0), which runs
// as a separate process. Only its data files are read; none of its code is used.
package eod2

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// LoadSymbolMap reads eod2_data/isin_symbol_map.json and returns ticker -> ISIN.
func LoadSymbolMap(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eod2 symbol map: %w", err)
	}
	var doc struct {
		Sym2ISIN map[string]string `json:"sym2isin"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("eod2 symbol map: %w", err)
	}
	if len(doc.Sym2ISIN) == 0 {
		return nil, errors.New("eod2 symbol map: no sym2isin entries")
	}
	return doc.Sym2ISIN, nil
}

// ParseDaily parses one eod2 daily file:
// Date,Open,High,Low,Close,Volume,Series,TOTAL_TRADES,QTY_PER_TRADE,DLV_QTY
func ParseDaily(r io.Reader, ticker, isin string) ([]market.Bar, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("eod2 %s: header: %w", ticker, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	required := []string{"Date", "Open", "High", "Low", "Close", "Volume", "Series"}
	for _, need := range required {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("eod2 %s: missing column %q", ticker, need)
		}
	}
	// minFields is one past the highest index any required column maps to. With
	// FieldsPerRecord = -1 (above) the csv reader tolerates ragged rows so eod2
	// files missing trailing optional columns (e.g. no DLV_QTY) still parse; this
	// guard is what catches a row that is short on the *required* columns instead
	// of panicking with an index-out-of-range.
	minFields := 0
	for _, need := range required {
		if col[need]+1 > minFields {
			minFields = col[need] + 1
		}
	}

	var bars []market.Bar
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("eod2 %s line %d: %w", ticker, line, err)
		}
		if len(rec) < minFields {
			return nil, fmt.Errorf("eod2 %s line %d: got %d fields, want at least %d", ticker, line, len(rec), minFields)
		}
		date, err := time.Parse("2006-01-02", rec[col["Date"]])
		if err != nil {
			return nil, fmt.Errorf("eod2 %s line %d: date: %w", ticker, line, err)
		}
		b := market.Bar{ISIN: isin, Ticker: ticker, Series: strings.TrimSpace(rec[col["Series"]]),
			Date: market.Day(date.Year(), date.Month(), date.Day())}
		var perr error
		b.Open, perr = number("Open", rec[col["Open"]], perr)
		b.High, perr = number("High", rec[col["High"]], perr)
		b.Low, perr = number("Low", rec[col["Low"]], perr)
		b.Close, perr = number("Close", rec[col["Close"]], perr)
		vol, perr := number("Volume", rec[col["Volume"]], perr)
		if perr != nil {
			return nil, fmt.Errorf("eod2 %s line %d: %w", ticker, line, perr)
		}
		b.Volume, perr = market.Count("Volume", vol)
		if perr != nil {
			return nil, fmt.Errorf("eod2 %s line %d: %w", ticker, line, perr)
		}
		if i, ok := col["DLV_QTY"]; ok && i < len(rec) && strings.TrimSpace(rec[i]) != "" {
			dlv, err := number("DLV_QTY", rec[i], nil)
			if err != nil {
				return nil, fmt.Errorf("eod2 %s line %d: %w", ticker, line, err)
			}
			q, err := market.Count("DLV_QTY", dlv)
			if err != nil {
				return nil, fmt.Errorf("eod2 %s line %d: %w", ticker, line, err)
			}
			b.DeliveryQty = &q
		}
		bars = append(bars, b)
	}
	return bars, nil
}

// number parses a float field, threading a prior error so callers check once.
// field names the column in the error message. A non-finite value is rejected
// here rather than stored: strconv.ParseFloat accepts "nan", "inf" and "-inf"
// with a nil error, and these CSVs are produced by a third-party process this
// repository does not control. See market.Finite.
func number(field, s string, prev error) (float64, error) {
	if prev != nil {
		return 0, prev
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if err := market.Finite(field, v); err != nil {
		return 0, err
	}
	return v, nil
}

// LoadDir parses every *.csv in dailyDir whose ticker (upper-cased file stem)
// resolves to an ISIN via sym2isin. Tickers without an ISIN are returned in
// skipped, sorted, so the caller can report them instead of silently losing data.
func LoadDir(dailyDir string, sym2isin map[string]string) ([]market.Bar, []string, error) {
	entries, err := os.ReadDir(dailyDir)
	if err != nil {
		return nil, nil, fmt.Errorf("eod2 daily dir: %w", err)
	}
	var bars []market.Bar
	var skipped []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".csv") {
			continue
		}
		ticker := strings.ToUpper(name[:len(name)-len(".csv")])
		isin, ok := sym2isin[ticker]
		if !ok {
			skipped = append(skipped, ticker)
			continue
		}
		f, err := os.Open(filepath.Join(dailyDir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		got, err := ParseDaily(f, ticker, isin)
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		bars = append(bars, got...)
	}
	sort.Strings(skipped)
	return bars, skipped, nil
}
