package nseindex

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// Summary counts what one Backfill run did.
type Summary struct {
	Fetched, NoFile, Errors, Skipped, Inserted int
}

// ist is India Standard Time; NSE publishes each session's index file that
// evening on this clock.
var ist = time.FixedZone("IST", 5*60*60+30*60)

// noFileSettleLag is how long after a session has ended a missing file stops
// meaning "not published yet" and starts meaning "there was no session".
//
// It is the same 24 hours the equity backfill uses and for the same reason:
// store.LoggedDates treats 'no-file' as permanently settled and never refetches
// it, so recording one for a session NSE simply has not posted yet would lose
// that session forever with no later run able to notice.
const noFileSettleLag = 24 * time.Hour

func noFileSettled(d, now time.Time) bool {
	endOfSession := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, ist).AddDate(0, 0, 1)
	return !now.Before(endOfSession.Add(noFileSettleLag))
}

// ArchiveStart is the earliest session the index archive serves. Dates before
// it answer with the soft-404 HTML page rather than a 404, and probing them is
// pure cost: a backfill from 2011 would spend six hundred requests learning
// what this constant records.
//
// Measured 2026-09-08: 2012-02-01 returns the HTML page and 2012-03-01 returns
// a CSV, so the boundary lies between them. The constant is deliberately the
// EARLIER of the two so a backfill still probes the gap rather than assuming
// where inside February the archive begins.
var ArchiveStart = market.Day(2012, 2, 1)

// Backfill walks every calendar date in [from, to], skipping dates already
// settled in ingest_log.
//
// Weekends are walked rather than skipped, for the reason M0 learned the hard
// way on the equity side: NSE holds live sessions on some Saturdays -- Muhurat,
// Budget days -- and a calendar-shaped skip drops them permanently because the
// date is never attempted, never logged, and no later run can see the hole. A
// weekend with no session simply comes back absent and settles down the same
// path a holiday takes.
func Backfill(ctx context.Context, store *market.Store, f *Fetcher, from, to time.Time, delay time.Duration, log io.Writer) (Summary, error) {
	var sum Summary
	done, err := store.LoggedDates(ctx, Source)
	if err != nil {
		return sum, err
	}
	if from.Before(ArchiveStart) {
		fmt.Fprintf(log, "note: the index archive starts %s; %s..%s will not be probed\n",
			ArchiveStart.Format(time.DateOnly), from.Format(time.DateOnly),
			ArchiveStart.AddDate(0, 0, -1).Format(time.DateOnly))
		from = ArchiveStart
	}

	startedAt := time.Now()
	consecutiveErrors := 0
	for d := market.Day(from.Year(), from.Month(), from.Day()); !d.After(to); d = d.AddDate(0, 0, 1) {
		if done[d] {
			sum.Skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		levels, found, err := f.Fetch(ctx, d)
		switch {
		case err != nil:
			sum.Errors++
			consecutiveErrors++
			fmt.Fprintf(log, "%s error: %v\n", d.Format(time.DateOnly), err)
			if lerr := store.LogIngest(ctx, Source, d, "error", 0, err.Error()); lerr != nil {
				return sum, lerr
			}
			if consecutiveErrors >= 5 {
				return sum, fmt.Errorf("aborting after %d consecutive errors", consecutiveErrors)
			}
		case !found:
			sum.NoFile++
			consecutiveErrors = 0
			if noFileSettled(d, startedAt) {
				if err := store.LogIngest(ctx, Source, d, "no-file", 0, "absent or soft-404 HTML"); err != nil {
					return sum, err
				}
			} else {
				fmt.Fprintf(log, "%s no file yet: left unlogged to retry\n", d.Format(time.DateOnly))
			}
		default:
			consecutiveErrors = 0
			n, err := store.InsertIndexLevels(ctx, Source, levels)
			if err != nil {
				return sum, fmt.Errorf("%s: insert: %w", d.Format(time.DateOnly), err)
			}
			sum.Fetched++
			sum.Inserted += n
			fmt.Fprintf(log, "%s ok: %d indices, %d new\n", d.Format(time.DateOnly), len(levels), n)
			if err := store.LogIngest(ctx, Source, d, "ok", len(levels), ""); err != nil {
				return sum, err
			}
		}
		select {
		case <-ctx.Done():
			return sum, ctx.Err()
		case <-time.After(delay):
		}
	}
	return sum, nil
}
