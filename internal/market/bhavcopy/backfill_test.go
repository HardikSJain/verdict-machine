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

// 2024-01-01 is a Monday, 2024-01-05 a Friday, 2024-01-08 the following
// Monday, so from..to spans exactly the weekdays used below with Jan 6-7
// (Sat/Sun) skipped by Backfill's own weekend check.

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
	to := market.Day(2024, 1, 8)   // the following Mon: 6 weekdays in range

	f, log := testServer(t, func(w http.ResponseWriter, date string) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	sum, err := Backfill(ctx, store, f, from, to, 0, io.Discard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "aborting after 5 consecutive errors")
	require.Equal(t, Summary{Errors: 5}, sum)
	require.Len(t, log.all(), 5, "the 6th weekday must never be attempted once the run aborts")
}

func TestBackfill_SuccessResetsConsecutiveErrorCounter(t *testing.T) {
	ctx := context.Background()
	store := market.NewStore(testutil.Pool(t))
	from := market.Day(2024, 1, 1)        // Mon: error 1
	successDate := market.Day(2024, 1, 5) // Fri: 4th weekday, succeeds and resets the counter
	to := market.Day(2024, 1, 8)          // Mon: error 5th overall, but only the 1st since the reset

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
	require.NoError(t, err, "a success between two runs of errors resets the counter, so 5 non-consecutive errors must not abort")
	require.Equal(t, Summary{Errors: 5, Fetched: 1, Inserted: 1}, sum)
	require.Len(t, log.all(), 6, "every weekday in range must be attempted; the run must not abort")
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
