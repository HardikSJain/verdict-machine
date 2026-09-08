// Command index-verify cross-checks the ingested NSE index archive against
// data it has no connection to, so the series can be trusted rather than
// assumed.
//
// `verdict index check` verifies identity -- that a rename left the level
// alone. This verifies the LEVELS, three ways, and the second is the one that
// matters:
//
//  1. The index archive's session calendar against the equity bhavcopy's. They
//     are different files published by different systems and must agree on
//     which days NSE traded; a disagreement means one of the two loaders is
//     inventing or dropping sessions.
//
//  2. Nifty 50 daily returns against NIFTYBEES, the ETF that tracks it. The
//     ETF's prices come from the equity bhavcopy and the index's from the index
//     archive -- two independent files, two independent loaders, two
//     independent identity schemes. If both are right they must move together
//     to within tracking error. Nothing in this repository forces that
//     agreement, which is exactly why it is worth measuring.
//
//  3. Spot closes against values documented outside this project.
//
// Read-only.
//
//	go run ./scripts/index-verify --database-url "$VERDICT_DATABASE_URL"
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HardikSJain/verdict-machine/internal/db"
	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/nseindex"
)

// knownCloses are Nifty 50 closing values documented outside this repository.
// They are deliberately a mix of ordinary and famous sessions: a loader that
// was off by a session, a scale factor or a whole rebasing would miss all of
// them, and one that merely lost a day would miss one.
var knownCloses = []struct {
	date  string
	close float64
	note  string
}{
	{"2020-03-23", 7610.25, "COVID crash low"},
	{"2021-10-18", 18477.05, "2021 peak"},
	{"2024-06-04", 21884.50, "election result day"},
	{"2016-11-08", 8543.55, "demonetisation announced after close"},
}

func main() {
	url := flag.String("database-url", os.Getenv("VERDICT_DATABASE_URL"), "Postgres URL")
	code := flag.String("index", nseindex.Nifty50, "index code")
	etf := flag.String("etf", "NIFTYBEES", "ETF ticker to cross-check against")
	tol := flag.Float64("tracking-tolerance", 0.01, "max mean absolute daily return difference vs the ETF")
	flag.Parse()
	if err := run(*url, *code, *etf, *tol); err != nil {
		fmt.Fprintln(os.Stderr, "index-verify:", err)
		os.Exit(1)
	}
}

func run(url, code, etf string, tol float64) error {
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
	pin := time.Now()
	var failures int

	// --- 1. coverage and the session calendar ------------------------------
	var first, last time.Time
	var sessions, levels int
	if err := pool.QueryRow(ctx, `
		SELECT min(date), max(date), count(DISTINCT date), count(*)
		FROM index_levels WHERE source = $1`, market.SourceNSEIndex).
		Scan(&first, &last, &sessions, &levels); err != nil {
		return err
	}
	fmt.Printf("index archive: %d levels over %d sessions, %s .. %s\n\n",
		levels, sessions, first.Format(time.DateOnly), last.Format(time.DateOnly))

	var bothDays, indexOnly, equityOnly int
	if err := pool.QueryRow(ctx, `
		WITH i AS (SELECT DISTINCT date FROM index_levels WHERE source=$1 AND date >= $3),
		     e AS (SELECT DISTINCT date FROM bars       WHERE source=$2 AND date >= $3)
		SELECT count(*) FILTER (WHERE i.date IS NOT NULL AND e.date IS NOT NULL),
		       count(*) FILTER (WHERE e.date IS NULL),
		       count(*) FILTER (WHERE i.date IS NULL)
		FROM i FULL OUTER JOIN e ON i.date = e.date`,
		market.SourceNSEIndex, market.SourceBhavcopy, first).Scan(&bothDays, &indexOnly, &equityOnly); err != nil {
		return err
	}
	fmt.Printf("session calendar vs equity bhavcopy over the same span:\n")
	fmt.Printf("  agree           %5d\n", bothDays)
	fmt.Printf("  index only      %5d\n", indexOnly)
	fmt.Printf("  equity only     %5d\n", equityOnly)
	if indexOnly > 0 || equityOnly > 0 {
		fmt.Printf("  -> the two files disagree about which days NSE traded; each one is a session\n")
		fmt.Printf("     one archive has and the other does not, and the filter must not read a\n")
		fmt.Printf("     missing index close as a signal\n")
		if err := printCalendarGaps(ctx, pool, first); err != nil {
			return err
		}
		failures++
	}
	fmt.Println()

	// --- 2. the index against the ETF that tracks it ----------------------
	idx, err := store.IndexCloses(ctx, code, first, last, pin)
	if err != nil {
		return err
	}
	etfCloses, err := etfSeries(ctx, pool, etf, first, last)
	if err != nil {
		return err
	}
	stat, err := trackingStats(idx, etfCloses)
	if err != nil {
		fmt.Printf("%s vs %s: %v\n\n", code, etf, err)
	} else {
		fmt.Printf("%s vs %s daily returns, %d overlapping sessions:\n", code, etf, stat.n)
		fmt.Printf("  correlation                 %.4f\n", stat.corr)
		fmt.Printf("  mean absolute difference    %.4f%%\n", 100*stat.mad)
		fmt.Printf("  worst single session        %.4f%% on %s\n", 100*stat.worst, stat.worstDate.Format(time.DateOnly))
		if len(stat.Excluded) > 0 {
			fmt.Printf("  excluded as corporate actions in %s (>%.0f%% in a day, which is not tracking error):\n",
				etf, 100*corporateActionThreshold)
			for _, d := range stat.Excluded {
				fmt.Printf("     %s\n", d.Format(time.DateOnly))
			}
		}
		if stat.corr < 0.95 || stat.mad > tol {
			fmt.Printf("  -> the index and the ETF that tracks it disagree; one of the two loaders is wrong\n")
			failures++
		} else {
			fmt.Printf("  -> two independent files, two loaders, same market\n")
		}
		fmt.Println()
	}

	// --- 3. spot checks against documented values -------------------------
	fmt.Printf("spot closes against values documented outside this project:\n")
	for _, k := range knownCloses {
		d, _ := time.Parse(time.DateOnly, k.date)
		d = market.Day(d.Year(), d.Month(), d.Day())
		if d.Before(first) || d.After(last) {
			fmt.Printf("  %s  (outside the ingested span)\n", k.date)
			continue
		}
		got, err := store.IndexCloses(ctx, code, d.AddDate(0, 0, -1), d, pin)
		if err != nil {
			return err
		}
		var have float64
		for _, c := range got {
			if c.Date.Equal(d) {
				have = c.Close
			}
		}
		diff := math.Abs(have-k.close) / k.close
		status := "ok"
		if have == 0 {
			status = "MISSING"
			failures++
		} else if diff > 0.001 {
			status = "MISMATCH"
			failures++
		}
		fmt.Printf("  %s  expected %10.2f  got %10.2f  %-8s %s\n", k.date, k.close, have, status, k.note)
	}

	fmt.Println()
	if failures > 0 {
		return fmt.Errorf("%d check(s) failed", failures)
	}
	fmt.Println("every cross-check passed")
	return nil
}

// printCalendarGaps names the disagreeing sessions. A count tells you
// something is wrong; the dates tell you which loader to look at, and whether
// it is a holiday one file honoured and the other did not or a session one of
// them simply missed.
func printCalendarGaps(ctx context.Context, pool *pgxpool.Pool, from time.Time) error {
	rows, err := pool.Query(ctx, `
		WITH i AS (SELECT DISTINCT date FROM index_levels WHERE source=$1 AND date >= $3),
		     e AS (SELECT DISTINCT date FROM bars       WHERE source=$2 AND date >= $3)
		SELECT coalesce(i.date, e.date) AS d,
		       CASE WHEN e.date IS NULL THEN 'index only' ELSE 'equity only' END AS side
		FROM i FULL OUTER JOIN e ON i.date = e.date
		WHERE i.date IS NULL OR e.date IS NULL
		ORDER BY d LIMIT 40`, market.SourceNSEIndex, market.SourceBhavcopy, from)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d time.Time
		var side string
		if err := rows.Scan(&d, &side); err != nil {
			return err
		}
		fmt.Printf("     %s  %s\n", d.Format(time.DateOnly), side)
	}
	return rows.Err()
}

func etfSeries(ctx context.Context, pool *pgxpool.Pool, ticker string, from, to time.Time) ([]market.DatedClose, error) {
	rows, err := pool.Query(ctx, `
		WITH em AS (SELECT symbol_id, entity_id FROM entity_map_at(now())),
		s AS (SELECT DISTINCT symbol_id, ticker FROM symbols WHERE ticker = $1)
		SELECT DISTINCT ON (b.date) b.date, b.close::float8
		FROM bars b JOIN s ON s.symbol_id = b.symbol_id
		WHERE b.source = $2 AND b.date >= $3 AND b.date <= $4
		ORDER BY b.date, b.ingested_at DESC`,
		ticker, market.SourceBhavcopy, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.DatedClose
	for rows.Next() {
		var d time.Time
		var c float64
		if err := rows.Scan(&d, &c); err != nil {
			return nil, err
		}
		out = append(out, market.DatedClose{Date: market.Day(d.Year(), d.Month(), d.Day()), Close: c})
	}
	return out, rows.Err()
}

type tracking struct {
	n         int
	corr      float64
	mad       float64
	worst     float64
	worstDate time.Time
	// Excluded are sessions where the two series disagree by more than a
	// quarter. An ETF tracking an index cannot miss by 25% in a day through
	// tracking error; that is a corporate action in the ETF, and leaving one in
	// destroys the statistic it is supposed to inform. NIFTYBEES did a 1:10
	// unit split on 2019-12-19 -- 1292.54 to 130.20 -- and that single session
	// dragged the correlation from 0.99 to 0.50.
	Excluded []time.Time
}

// corporateActionThreshold is what counts as "not tracking error".
const corporateActionThreshold = 0.25

// trackingStats compares two series' DAILY RETURNS rather than their levels.
// Levels cannot be compared: the ETF is roughly a hundredth of the index and
// accumulates dividends, so it drifts steadily above. Returns have no such
// scale and no such drift, and they are what a trend rule actually reads.
func trackingStats(a, b []market.DatedClose) (tracking, error) {
	byDate := map[string]float64{}
	for _, c := range b {
		byDate[c.Date.Format(time.DateOnly)] = c.Close
	}
	type pair struct {
		d    time.Time
		x, y float64
	}
	var joined []pair
	for _, c := range a {
		if v, ok := byDate[c.Date.Format(time.DateOnly)]; ok {
			joined = append(joined, pair{c.Date, c.Close, v})
		}
	}
	sort.Slice(joined, func(i, j int) bool { return joined[i].d.Before(joined[j].d) })
	if len(joined) < 30 {
		return tracking{}, fmt.Errorf("only %d overlapping sessions; not enough to compare", len(joined))
	}
	var xs, ys []float64
	var dates []time.Time
	for i := 1; i < len(joined); i++ {
		if joined[i-1].x <= 0 || joined[i-1].y <= 0 {
			continue
		}
		xs = append(xs, joined[i].x/joined[i-1].x-1)
		ys = append(ys, joined[i].y/joined[i-1].y-1)
		dates = append(dates, joined[i].d)
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(len(xs))
	my /= float64(len(ys))
	// Drop corporate actions before computing anything, including the means.
	var fx, fy []float64
	var fd, excluded []time.Time
	for i := range xs {
		if math.Abs(xs[i]-ys[i]) > corporateActionThreshold {
			excluded = append(excluded, dates[i])
			continue
		}
		fx = append(fx, xs[i])
		fy = append(fy, ys[i])
		fd = append(fd, dates[i])
	}
	if len(fx) < 30 {
		return tracking{}, fmt.Errorf("only %d comparable sessions after excluding corporate actions", len(fx))
	}
	mx, my = 0, 0
	for i := range fx {
		mx += fx[i]
		my += fy[i]
	}
	mx /= float64(len(fx))
	my /= float64(len(fy))

	var cov, vx, vy, sad, worst float64
	var worstDate time.Time
	for i := range fx {
		dx, dy := fx[i]-mx, fy[i]-my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
		d := math.Abs(fx[i] - fy[i])
		sad += d
		if d > worst {
			worst, worstDate = d, fd[i]
		}
	}
	return tracking{
		n: len(fx), corr: cov / math.Sqrt(vx*vy), mad: sad / float64(len(fx)),
		worst: worst, worstDate: worstDate, Excluded: excluded,
	}, nil
}
