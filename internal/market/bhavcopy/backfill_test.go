package bhavcopy

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// legacyCSV builds a minimal legacy-layout bhavcopy CSV (header + EQ rows),
// enough for Parse to dispatch on SYMBOL/SERIES/TIMESTAMP and keep every row.
func legacyCSV(rows ...string) string {
	return "SYMBOL,SERIES,OPEN,HIGH,LOW,CLOSE,TOTTRDQTY,TOTTRDVAL,TIMESTAMP,ISIN\n" + strings.Join(rows, "")
}

func legacyRow(d time.Time, ticker, isin string, close float64) string {
	return fmt.Sprintf("%s,EQ,100.00,110.00,90.00,%.2f,1000,105000.00,%s,%s\n",
		ticker, close, d.Format("02-Jan-2006"), isin)
}

// zipCSV zips content under name, the way NSE ships one CSV per archive.
func zipCSV(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	require.NoError(t, err)
	_, err = w.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// requestLog records, in order, the date path each request to a testServer
// carried, so a test can assert exactly which dates were (or were not) fetched.
type requestLog struct {
	mu    sync.Mutex
	dates []string
}

func (l *requestLog) add(d string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dates = append(l.dates, d)
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.dates))
	copy(out, l.dates)
	return out
}

// testServer returns a Fetcher backed by an httptest.Server whose response
// for each request is decided by handle, plus the log of dates requested.
func testServer(t *testing.T, handle func(w http.ResponseWriter, date string)) (*Fetcher, *requestLog) {
	t.Helper()
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		date := strings.TrimPrefix(r.URL.Path, "/")
		log.add(date)
		handle(w, date)
	}))
	t.Cleanup(srv.Close)
	f := &Fetcher{
		Client:    srv.Client(),
		UserAgent: "backfill-test",
		URLFor:    func(d time.Time) string { return srv.URL + "/" + d.Format("2006-01-02") },
	}
	return f, log
}

// 2024-01-01 is a Monday, 2024-01-05 a Friday, 2024-01-06 and 2024-01-07 the
// weekend, 2024-01-08 the following Monday. Backfill walks all eight dates:
// it has no weekday filter, because NSE holds real sessions on some weekends.

func TestBackfill_SkipsAlreadyLoggedDate(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d := market.Day(2024, 1, 1)
	require.NoError(t, store.LogIngest(ctx, market.SourceBhavcopy, d, "ok", 10, ""))

	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		t.Fatalf("unexpected fetch for %s; a date already logged ok must be skipped, not refetched", date)
	})

	sum, err := Backfill(ctx, store, f, d, d, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{Skipped: 1}, sum)
	require.Empty(t, log.all())
}

func TestBackfill_RetriesPreviouslyLoggedErrorDate(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d := market.Day(2024, 1, 2)
	require.NoError(t, store.LogIngest(ctx, market.SourceBhavcopy, d, "error", 0, "timeout"))

	payload := zipCSV(t, "bhav.csv", legacyCSV(legacyRow(d, "TESTCO", "TESTISIN0001", 105)))
	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	})

	sum, err := Backfill(ctx, store, f, d, d, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{Fetched: 1, Inserted: 1}, sum)
	require.Equal(t, []string{"2024-01-02"}, log.all(), "a previously-errored date is retried, not skipped")
}

func TestBackfill_404LogsNoFileAndDoesNotAbort(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	holiday := market.Day(2024, 1, 1)
	trading := market.Day(2024, 1, 2)

	payload := zipCSV(t, "bhav.csv", legacyCSV(legacyRow(trading, "TESTCO", "TESTISIN0001", 105)))
	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		if date != trading.Format("2006-01-02") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	})

	sum, err := Backfill(ctx, store, f, holiday, trading, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{NoFile: 1, Fetched: 1, Inserted: 1}, sum)
	require.Equal(t, []string{"2024-01-01", "2024-01-02"}, log.all())

	done, err := store.LoggedDates(ctx, market.SourceBhavcopy)
	require.NoError(t, err)
	require.True(t, done[holiday], "a 404 is settled as no-file")
	require.True(t, done[trading])
}

func TestBackfill_AbortsAfterFiveConsecutiveErrors(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	from := market.Day(2024, 1, 1) // Mon
	to := market.Day(2024, 1, 8)   // the following Mon: 8 calendar dates in range

	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	sum, err := Backfill(ctx, store, f, from, to, 0, io.Discard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "aborting after 5 consecutive errors")
	require.Equal(t, Summary{Errors: 5}, sum)
	require.Len(t, log.all(), 5, "the 6th date must never be attempted once the run aborts")
}

func TestBackfill_SuccessResetsConsecutiveErrorCounter(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	from := market.Day(2024, 1, 1)        // Mon: error 1
	successDate := market.Day(2024, 1, 5) // Fri: 5th date, succeeds and resets the counter
	to := market.Day(2024, 1, 8)          // Mon: 7 errors overall, but never 5 in a row

	payload := zipCSV(t, "bhav.csv", legacyCSV(legacyRow(successDate, "TESTCO", "TESTISIN0001", 105)))
	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		if date == successDate.Format("2006-01-02") {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(payload)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})

	sum, err := Backfill(ctx, store, f, from, to, 0, io.Discard)
	require.NoError(t, err, "a success between two runs of errors resets the counter, so 7 errors that never reach 5 in a row must not abort")
	require.Equal(t, Summary{Errors: 7, Fetched: 1, Inserted: 1}, sum)
	require.Len(t, log.all(), 8, "every calendar date in range must be attempted; the run must not abort")
}

func TestBackfill_SummaryCountersMatchMixedScenario(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d1 := market.Day(2024, 1, 1) // pre-logged ok -> skipped
	d2 := market.Day(2024, 1, 2) // 404 -> no-file
	d3 := market.Day(2024, 1, 3) // ok, 2 bars
	d4 := market.Day(2024, 1, 4) // error, does not abort (a single consecutive error)
	d5 := market.Day(2024, 1, 5) // ok, 1 bar

	require.NoError(t, store.LogIngest(ctx, market.SourceBhavcopy, d1, "ok", 5, ""))

	d3Payload := zipCSV(t, "bhav.csv", legacyCSV(
		legacyRow(d3, "AAA", "TESTISIN0001", 100),
		legacyRow(d3, "BBB", "TESTISIN0002", 200),
	))
	d5Payload := zipCSV(t, "bhav.csv", legacyCSV(legacyRow(d5, "CCC", "TESTISIN0003", 300)))

	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		switch date {
		case d3.Format("2006-01-02"):
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(d3Payload)
		case d5.Format("2006-01-02"):
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(d5Payload)
		case d4.Format("2006-01-02"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	sum, err := Backfill(ctx, store, f, d1, d5, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{Skipped: 1, NoFile: 1, Fetched: 2, Errors: 1, Inserted: 3}, sum)
	require.Equal(t,
		[]string{d2.Format("2006-01-02"), d3.Format("2006-01-02"), d4.Format("2006-01-02"), d5.Format("2006-01-02")},
		log.all(), "d1 was already logged ok and must not be fetched")
}

func TestNoFileSettled_HoldsUntilNSEsPublishWindowHasPassed(t *testing.T) {
	session := market.Day(2024, 1, 1)
	// The session ends at 2024-01-02 00:00 IST; a 404 only settles a further
	// noFileSettleLag later, at 2024-01-03 00:00 IST == 2024-01-02 18:30 UTC.
	cutoff := time.Date(2024, 1, 2, 18, 30, 0, 0, time.UTC)

	require.False(t, noFileSettled(session, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)),
		"midday on the session itself: NSE publishes in the evening IST, so a 404 proves nothing yet")
	require.False(t, noFileSettled(session, cutoff.Add(-time.Second)),
		"one second before the cutoff the date is still retryable")
	require.True(t, noFileSettled(session, cutoff),
		"at the cutoff a 404 means there was no session")
	require.True(t, noFileSettled(session, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		"a long-past date settles")
}

// unpublishedSession returns a date NSE cannot yet be assumed to have
// published: today in IST. Backfill walks weekend dates too, so today serves
// whatever day of the week the suite happens to run on.
func unpublishedSession(now time.Time) time.Time {
	d := now.In(ist)
	return market.Day(d.Year(), d.Month(), d.Day())
}

func TestBackfill_UnpublishedSessionIsNotSettledAndIsRetried(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	d := unpublishedSession(time.Now())
	require.False(t, noFileSettled(d, time.Now()), "the test date must be inside the publish window")
	ds := d.Format("2006-01-02")

	// NSE has not published this session yet, so the archive 404s exactly as a
	// holiday would.
	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		w.WriteHeader(http.StatusNotFound)
	})

	sum, err := Backfill(ctx, store, f, d, d, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{NoFile: 1}, sum)

	done, err := store.LoggedDates(ctx, market.SourceBhavcopy)
	require.NoError(t, err)
	require.False(t, done[d],
		"a 404 for a session NSE may simply not have published yet must not be settled as no-file")

	// A later run -- the operator waiting for the evening publish -- must try
	// again rather than skip the date forever.
	sum, err = Backfill(ctx, store, f, d, d, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{NoFile: 1}, sum, "still unsettled, so fetched again rather than skipped")
	require.Equal(t, []string{ds, ds}, log.all(), "the second run refetches the unsettled date")
}

func TestBackfill_SettledNoFileStillLogsAndSkipsOnRerun(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	holiday := market.Day(2024, 1, 1) // long past: a 404 here really is a holiday
	require.True(t, noFileSettled(holiday, time.Now()))

	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		w.WriteHeader(http.StatusNotFound)
	})

	sum, err := Backfill(ctx, store, f, holiday, holiday, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{NoFile: 1}, sum)

	sum, err = Backfill(ctx, store, f, holiday, holiday, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{Skipped: 1}, sum, "a settled holiday is never refetched")
	require.Equal(t, []string{"2024-01-01"}, log.all())
}

// TestBackfill_FetchesWeekendSessions pins the absence of a weekday filter.
// NSE holds live sessions on some Saturdays and Sundays -- Muhurat trading,
// Budget Saturdays, special live-trading and disaster-recovery sessions --
// and 2024-01-20 was one of them (a Saturday special session). A
// time.Weekday()-driven skip fired before ingest_log was read or written, so
// such a date was never attempted, never logged, and no later run could
// recover it: the bars were permanently and silently absent from the source
// of record. The Sunday beside it stands for the ordinary case and must
// settle as 'no-file' exactly as a holiday does.
func TestBackfill_FetchesWeekendSessions(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	saturday := market.Day(2024, 1, 20)
	sunday := market.Day(2024, 1, 21)
	require.Equal(t, time.Saturday, saturday.Weekday())
	require.Equal(t, time.Sunday, sunday.Weekday())

	payload := zipCSV(t, "bhav.csv", legacyCSV(legacyRow(saturday, "TESTCO", "TESTISIN0001", 105)))
	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		if date != saturday.Format("2006-01-02") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	})

	sum, err := Backfill(ctx, store, f, saturday, sunday, 0, io.Discard)
	require.NoError(t, err)
	require.Equal(t, Summary{Fetched: 1, NoFile: 1, Inserted: 1}, sum)
	require.Equal(t, []string{"2024-01-20", "2024-01-21"}, log.all(),
		"both weekend dates must be attempted; a weekday filter would fetch neither")

	bars, err := store.BarsForDate(ctx, market.SourceBhavcopy, saturday, time.Now())
	require.NoError(t, err)
	require.Len(t, bars, 1, "the Saturday session's bars reach the store")

	done, err := store.LoggedDates(ctx, market.SourceBhavcopy)
	require.NoError(t, err)
	require.True(t, done[saturday])
	require.True(t, done[sunday], "a weekend with no session settles as no-file like any holiday")
}
