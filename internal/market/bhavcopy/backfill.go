package bhavcopy

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// Summary counts what one Backfill run did. NoFile counts every date NSE had
// no archive for during this run, whether that absence was settled in
// ingest_log or left unlogged for a later retry (see noFileSettled).
type Summary struct {
	Fetched, NoFile, Errors, Skipped, Inserted int
}

// ist is India Standard Time. NSE's sessions, and the evening publish of each
// session's bhavcopy archive, both run on this clock.
var ist = time.FixedZone("IST", 5*60*60+30*60)

// noFileSettleLag is how long after a session date has ended (midnight IST) a
// 404 stops meaning "NSE has not published this session yet" and starts
// meaning "there was no session". NSE posts each day's bhavcopy that same
// evening IST (the evening flow gives up at 20:30 IST), so a further full day
// of slack absorbs a late publish while still settling genuine holidays on the
// next day's run.
const noFileSettleLag = 24 * time.Hour

// noFileSettled reports whether a 404 for session date d, observed at now, is
// safe to record as a terminal 'no-file' row. It matters because
// store.LoggedDates treats 'no-file' exactly like 'ok': permanently settled,
// never refetched. A 404 for a date NSE simply has not published yet -- the
// current trading day, fetched before the evening publish -- is therefore
// indistinguishable from a holiday once written, and no later run could ever
// correct it. Dates still inside that window are left unlogged instead, so the
// next run retries them.
func noFileSettled(d, now time.Time) bool {
	endOfSession := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, ist).AddDate(0, 0, 1)
	return !now.Before(endOfSession.Add(noFileSettleLag))
}

// Backfill walks every weekday from `from` to `to` inclusive, skipping dates
// already settled in ingest_log, fetching the rest with `delay` between
// requests. Errors are logged and retried on the next run; five consecutive
// errors abort the run so a blocked client does not hammer NSE. A 404 for a
// date too recent to be sure NSE has published it yet is also retried on the
// next run rather than settled as 'no-file'.
func Backfill(ctx context.Context, store *market.Store, f *Fetcher, from, to time.Time, delay time.Duration, log io.Writer) (Summary, error) {
	var sum Summary
	done, err := store.LoggedDates(ctx, market.SourceBhavcopy)
	if err != nil {
		return sum, err
	}
	// One clock reading for the whole run: a long backfill must not settle a
	// date it started too early to judge just because it is still going.
	startedAt := time.Now()
	consecutiveErrors := 0
	for d := market.Day(from.Year(), from.Month(), from.Day()); !d.After(to); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		if done[d] {
			sum.Skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		bars, found, err := f.Fetch(ctx, d)
		switch {
		case err != nil:
			sum.Errors++
			consecutiveErrors++
			fmt.Fprintf(log, "%s error: %v\n", d.Format("2006-01-02"), err)
			if lerr := store.LogIngest(ctx, market.SourceBhavcopy, d, "error", 0, err.Error()); lerr != nil {
				return sum, lerr
			}
			if consecutiveErrors >= 5 {
				return sum, fmt.Errorf("aborting after %d consecutive errors", consecutiveErrors)
			}
		case !found:
			sum.NoFile++
			consecutiveErrors = 0
			if noFileSettled(d, startedAt) {
				if err := store.LogIngest(ctx, market.SourceBhavcopy, d, "no-file", 0, "HTTP 404"); err != nil {
					return sum, err
				}
			} else {
				fmt.Fprintf(log, "%s no file yet: NSE may not have published this session; left unlogged to retry\n",
					d.Format("2006-01-02"))
			}
		default:
			consecutiveErrors = 0
			n, err := store.InsertBars(ctx, market.SourceBhavcopy, bars)
			if err != nil {
				return sum, fmt.Errorf("%s: insert: %w", d.Format("2006-01-02"), err)
			}
			sum.Fetched++
			sum.Inserted += n
			fmt.Fprintf(log, "%s ok: %d bars, %d new\n", d.Format("2006-01-02"), len(bars), n)
			if err := store.LogIngest(ctx, market.SourceBhavcopy, d, "ok", len(bars), ""); err != nil {
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
