// Command archive-probe fetches a few sessions straight from NSE and reports
// what came back, so a claim about the archive is a measurement rather than a
// memory.
//
// It exists because the answer to "how far back does the data go" was wrong
// for a year. This project ingests from 2011-09-02, which reads like a choice
// and is not one: NSE began printing an ISIN column somewhere between
// 2011-06-01 and 2011-07-04, everything here keys on ISIN, and so the archive
// simply started where identity did. The exchange has served daily files since
// its first session on 1994-11-03.
//
// Run it after touching the fetcher or either parser layout. The interesting
// column is "with ISIN": it must be zero before July 2011 and equal to the bar
// count after, and a layout change that breaks the dispatch shows up here as
// fifteen years of silently discarded identities.
//
//	go run ./scripts/archive-probe
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/bhavcopy"
)

func main() {
	f := bhavcopy.NewFetcher()
	for _, d := range []time.Time{
		market.Day(2007, 10, 1), market.Day(2008, 1, 2), market.Day(2008, 10, 24),
		market.Day(2010, 6, 1), market.Day(2011, 7, 4), market.Day(2015, 6, 30),
	} {
		bars, found, err := f.Fetch(context.Background(), d)
		if err != nil || !found {
			fmt.Printf("  %s  no file (err=%v)\n", d.Format(time.DateOnly), err)
			continue
		}
		withISIN := 0
		for _, b := range bars {
			if b.ISIN != "" {
				withISIN++
			}
		}
		fmt.Printf("  %s  %5d EQ bars, %5d with ISIN   first=%-12s close=%.2f\n",
			d.Format(time.DateOnly), len(bars), withISIN, bars[0].Ticker, bars[0].Close)
	}
}
