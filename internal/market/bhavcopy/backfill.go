package bhavcopy

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

// Backfill walks every weekday from `from` to `to` inclusive, skipping dates
// already settled in ingest_log, fetching the rest with `delay` between
// requests. Errors are logged and retried on the next run; five consecutive
// errors abort the run so a blocked client does not hammer NSE.
func Backfill(ctx context.Context, store *market.Store, f *Fetcher, from, to time.Time, delay time.Duration, log io.Writer) (Summary, error) {
	var sum Summary
	done, err := store.LoggedDates(ctx, market.SourceBhavcopy)
	if err != nil {
		return sum, err
	}
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
			if err := store.LogIngest(ctx, market.SourceBhavcopy, d, "no-file", 0, "HTTP 404"); err != nil {
				return sum, err
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
