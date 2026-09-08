// Command fence-cost answers the only question that decides whether the
// return fence is usable: how many names does it actually refuse?
//
// A fence that refuses everything and a fence that refuses nothing are both
// useless, and neither is distinguishable from a correct one by reading the
// code. So this runs the shipping EntityReturns over a real point-in-time
// universe with a real 12-1 momentum window and prints the split, plus every
// refused name with the corporate action that refused it.
//
// It is committed for the same reason scripts/read-cost is: docs/DESIGN.md
// quotes a number from it, and the next reader should be able to re-run that
// number rather than believe it.
//
// Read-only.
//
//	go run ./scripts/fence-cost --database-url "$VERDICT_DATABASE_URL"
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
	asOfS := flag.String("as-of", "2026-09-04", "rebalance session (YYYY-MM-DD)")
	lookback := flag.Int("lookback", 125, "universe lookback in sessions")
	n := flag.Int("n", 500, "universe size")
	skipMonths := flag.Int("skip", 1, "months skipped at the near end (the 1 of 12-1)")
	formMonths := flag.Int("formation", 12, "months from the far end (the 12 of 12-1)")
	show := flag.Int("show", 20, "refused names to print")
	flag.Parse()

	if err := run(*url, *source, *asOfS, *lookback, *n, *formMonths, *skipMonths, *show); err != nil {
		fmt.Fprintln(os.Stderr, "fence-cost:", err)
		os.Exit(1)
	}
}

func run(url, source, asOfS string, lookback, n, formMonths, skipMonths, show int) error {
	if url == "" {
		return fmt.Errorf("set --database-url or VERDICT_DATABASE_URL")
	}
	asOf, err := time.Parse(time.DateOnly, asOfS)
	if err != nil {
		return err
	}
	asOf = market.Day(asOf.Year(), asOf.Month(), asOf.Day())

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := market.NewStore(pool)

	// One pin for the whole run: the universe, the entity map, the bars and
	// the boundary set all have to answer the same question.
	pin := time.Now()

	uni, err := store.UniverseAsOf(ctx, source, asOf, lookback, n, pin)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(uni.Members))
	label := map[int64]string{}
	for _, m := range uni.Members {
		ids = append(ids, m.EntityID)
		label[m.EntityID] = m.Ticker
	}

	from := asOf.AddDate(0, -formMonths, 0)
	to := asOf.AddDate(0, -skipMonths, 0)

	start := time.Now()
	rs, err := store.EntityReturns(ctx, source, ids, from, to, pin)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)

	fmt.Printf("as-of %s  source %s  universe %d (lookback %d)\n",
		asOf.Format(time.DateOnly), source, len(ids), uni.Sessions)
	fmt.Printf("%d-%d momentum window %s .. %s\n",
		formMonths, skipMonths, from.Format(time.DateOnly), to.Format(time.DateOnly))
	fmt.Printf("guard band %d sessions, endpoint staleness %d days, read %s\n\n",
		market.BoundaryGuardSessions, market.EndpointStaleDays, elapsed.Round(time.Millisecond))

	fmt.Printf("  priced   %4d  (%.1f%%)\n", len(rs.Priced), pct(len(rs.Priced), len(ids)))
	fmt.Printf("  refused  %4d  (%.1f%%)  succession boundary inside the window\n",
		len(rs.Refused), pct(len(rs.Refused), len(ids)))
	fmt.Printf("  absent   %4d  (%.1f%%)  no session at an endpoint\n\n",
		len(rs.Absent), pct(len(rs.Absent), len(ids)))

	refused := make([]market.BoundaryRefusal, 0, len(rs.Refused))
	for _, r := range rs.Refused {
		refused = append(refused, r)
	}
	sort.Slice(refused, func(i, j int) bool {
		return refused[i].Boundary.Date.After(refused[j].Boundary.Date)
	})
	if len(refused) > 0 {
		fmt.Println("refused, most recent boundary first:")
		for i, r := range refused {
			if i >= show {
				fmt.Printf("  ... and %d more\n", len(refused)-show)
				break
			}
			fmt.Printf("  %-14s boundary %s  guard from %s  %s -> %s\n",
				label[r.EntityID], r.Boundary.Date.Format(time.DateOnly),
				r.GuardStart.Format(time.DateOnly),
				r.Boundary.PredecessorISIN, r.Boundary.SuccessorISIN)
		}
	}
	return rs.Accounted(ids)
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}
