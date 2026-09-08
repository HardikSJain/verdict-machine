// Command read-cost times the two reads that pay for the canonical-entity
// overlay, so the figures docs/DESIGN.md quotes for them can be re-run rather
// than believed.
//
// It exists because the first published pair -- "UniverseAsOf 578 ms -> 625 ms"
// -- was measured with `symbol_links` shadowed as an empty CTE, because the
// table did not exist on the live store yet. It does now, and an empty CTE is
// not an empty table with two indexes. Judgement call 5 in docs/DESIGN.md
// publishes its SQL beside its number; this is the same courtesy for a figure
// that cannot be written as one query.
//
// Read-only. It runs the shipping Go builders against whatever store it is
// pointed at and prints every run plus the median, because one warm number
// from one machine is not a measurement.
//
//	go run ./scripts/read-cost --database-url "$VERDICT_DATABASE_URL"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/db"
	"github.com/HardikSJain/verdict-machine/internal/market"
)

func main() {
	url := flag.String("database-url", os.Getenv("VERDICT_DATABASE_URL"), "Postgres URL")
	source := flag.String("source", market.SourceBhavcopy, "bar source")
	asOfS := flag.String("as-of", "2026-09-04", "session to read (YYYY-MM-DD)")
	lookback := flag.Int("lookback", 125, "universe lookback in sessions")
	n := flag.Int("n", 500, "universe size")
	runs := flag.Int("runs", 8, "timed runs per read, after one warm-up")
	flag.Parse()

	if err := run(*url, *source, *asOfS, *lookback, *n, *runs); err != nil {
		fmt.Fprintln(os.Stderr, "read-cost:", err)
		os.Exit(1)
	}
}

func run(url, source, asOfS string, lookback, n, runs int) error {
	asOf, err := time.Parse("2006-01-02", asOfS)
	if err != nil {
		return fmt.Errorf("--as-of: %w", err)
	}
	if url == "" {
		return fmt.Errorf("set --database-url or VERDICT_DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := market.NewStore(pool)

	// The pin is taken once and reused by both reads, so a row ingested
	// mid-measurement cannot make one run read a different store from the
	// next. Every timing below is of the SAME query against the SAME data.
	pin := time.Now()

	if err := timeRead(fmt.Sprintf("UniverseAsOf(%s, %s, lookback %d, n %d)", source, asOfS, lookback, n), runs, func() (string, error) {
		u, err := store.UniverseAsOf(ctx, source, asOf, lookback, n, pin)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d members, %d sessions", len(u.Members), u.Sessions), nil
	}); err != nil {
		return err
	}
	return timeRead(fmt.Sprintf("BarsForDate(%s, %s)", source, asOfS), runs, func() (string, error) {
		bars, err := store.BarsForDate(ctx, source, asOf, pin)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d bars", len(bars)), nil
	})
}

// timeRead runs read once to warm the caches, then runs times, and prints every
// sample beside the median. A single number hides the spread, and the spread
// is most of what a reader needs in order to know whether a 47 ms difference
// between two figures means anything.
func timeRead(label string, runs int, read func() (string, error)) error {
	what, err := read()
	if err != nil {
		return err
	}
	took := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		started := time.Now()
		if _, err := read(); err != nil {
			return err
		}
		took = append(took, time.Since(started))
	}
	sorted := append([]time.Duration(nil), took...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	fmt.Printf("%s -> %s\n", label, what)
	for _, d := range took {
		fmt.Printf("  %s\n", d.Round(time.Millisecond))
	}
	fmt.Printf("  median of %d: %s\n", runs, sorted[len(sorted)/2].Round(time.Millisecond))
	return nil
}
