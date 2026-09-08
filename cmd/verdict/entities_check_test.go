package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/testutil"
)

// `verdict entities check` against a real store: the exit code the monthly ops
// item is built on.
//
// Design 4.6 gives check two jobs, and 9's runbook hangs on the second --
// "exits non-zero on any accepted candidate the map is missing". Everything
// underneath that sentence was already tested in pieces (UnlinkedCandidates
// against a real store and the real Apply, EntityMapAt against a real map);
// the command that joins them was smoked by hand and by nothing else. A
// wiring mistake -- scanCandidates' count dropped on the floor, the final
// switch reordered, the scan quietly behind a flag -- leaves every unit test
// green while the monthly item becomes a procedure that cannot fail, which is
// the precise failure the candidate half was built to remove. These are the
// three states an operator can be in: the seed has fallen behind, the seed is
// current, and the scan was deliberately not run.

const (
	checkPred = "INE081A01012" // TATASTEEL before its face-value split
	checkSucc = "INE081A01020" // and after it
	checkCtrl = "INE666D01014" // a name that never changed ISIN
)

var (
	// One synthetic trading year whose calendar is plain weekdays: what is
	// under test is the command's exit code, not NSE's holiday list.
	checkFrom     = market.Day(2017, 1, 2)
	checkTo       = market.Day(2017, 12, 29)
	checkPredLast = market.Day(2017, 7, 28)
	checkBoundary = market.Day(2017, 7, 31)
	// Late enough that every date in the span has passed its settlement
	// horizon, so the archive reads as complete rather than as pending and
	// G4a lets the generator run at all.
	checkFetchedAt = time.Date(2018, 1, 5, 10, 0, 0, 0, time.UTC)
)

// seedCheckStore builds one clean succession -- disjoint spans one session
// apart, same NSDL issuer, serial 01 -> 02, boundary close ratio 107.60/100.35
// inside the 1/1 band -- plus a control name and a fully settled ingest_log,
// and returns the URL the CLI should be pointed at. Without all of that the
// gates quarantine the pair and there is no accepted candidate to miss.
func seedCheckStore(t *testing.T) string {
	t.Helper()
	pool := testutil.Pool(t)
	store := market.NewStore(pool)
	checkSeries(t, store, checkCtrl, "CONTROL", checkFrom, checkTo, 100, 100)
	checkSeries(t, store, checkPred, "TATASTEEL", checkFrom, checkPredLast, 959.4, 100.35)
	checkSeries(t, store, checkSucc, "TATASTEEL", checkBoundary, checkTo, 107.6, 110)
	settleCheckArchive(t, pool)
	return os.Getenv("VERDICT_TEST_DATABASE_URL")
}

// checkSeries writes one symbol's bars over the weekdays of a span at a flat
// close, with the first session's close overridden so the boundary ratio can
// be aimed at a canonical band.
func checkSeries(t *testing.T, store *market.Store, isin, ticker string, from, to time.Time, firstClose, close float64) {
	t.Helper()
	turnover := 5_000_000.0
	var bars []market.Bar
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		c := close
		if len(bars) == 0 {
			c = firstClose
		}
		bars = append(bars, market.Bar{
			ISIN: isin, Ticker: ticker, Series: "EQ", Date: d,
			Open: c, High: c, Low: c, Close: c,
			Volume: 1000, Turnover: &turnover,
		})
	}
	_, err := store.InsertBars(context.Background(), market.SourceBhavcopy, bars)
	require.NoError(t, err)
}

// settleCheckArchive logs every calendar date in the span as fetched, so the
// span holds no hole. G4a refuses to emit candidates at all while one does,
// and that refusal is a different non-zero exit from the one under test here.
func settleCheckArchive(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for d := checkFrom; !d.After(checkTo); d = d.AddDate(0, 0, 1) {
		status := "ok"
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			status = "no-file"
		}
		_, err := pool.Exec(context.Background(),
			`INSERT INTO ingest_log (source, date, status, rows, fetched_at) VALUES ($1, $2, $3, 1, $4)`,
			market.SourceBhavcopy, d, status, checkFetchedAt)
		require.NoError(t, err)
	}
}

// TestEntitiesCheckCommand_ExitsNonZeroOnAnUnlinkedAcceptedCandidate is the
// monthly item going red, which is the only thing that makes it a control
// rather than a calendar entry.
func TestEntitiesCheckCommand_ExitsNonZeroOnAnUnlinkedAcceptedCandidate(t *testing.T) {
	url := seedCheckStore(t)

	out, err := runCmd(t, "entities", "check", "--database-url", url)
	require.Error(t, err,
		"the gates accept this succession and the map has never been told about it; a zero exit here is a monthly procedure that cannot fail")
	require.ErrorContains(t, err, "1 accepted candidate(s) the map does not carry")
	require.ErrorContains(t, err, "verdict entities propose",
		"the error has to say what to do next: the operator reading it is a month away from the last time they ran this")
	require.Contains(t, out, "UNLINKED\tcandidate\t"+checkPred+" -> "+checkSucc)
	require.Contains(t, out, "boundary "+checkBoundary.Format("2006-01-02"),
		"the boundary is what a reviewer checks the pair against, so it is printed with the finding")
	require.Contains(t, out, "# 1 accepted candidate(s) the map does not carry, 0 quarantined candidate(s)")

	// The ordering docs/DESIGN.md claims and nothing else pinned: the
	// invariant report is printed BEFORE the candidate scan can fail, so an
	// operator whose scan blows up still has the answer about the map that
	// was already computed.
	require.Contains(t, out, "# 0 violations")
	require.Less(t, strings.Index(out, "# 0 violations"), strings.Index(out, "UNLINKED"),
		"the invariant report must survive a failing candidate scan; printing it afterwards would withhold an answer the command already had")
}

// TestEntitiesCheckCommand_GoesQuietOnceTheRosterIsApplied runs the whole
// monthly runbook through the CLI -- propose, apply, check -- because a check
// that stays red after the fix is one an operator stops reading, and because
// the runbook's three commands have never been executed in sequence by a test.
func TestEntitiesCheckCommand_GoesQuietOnceTheRosterIsApplied(t *testing.T) {
	url := seedCheckStore(t)
	dir := t.TempDir()
	rosterPath := filepath.Join(dir, "roster.json")

	out, err := runCmd(t, "entities", "propose", "--database-url", url,
		"--out", rosterPath, "--review-out", filepath.Join(dir, "review.jsonl"))
	require.NoError(t, err)
	require.Contains(t, out, "accepted\t1")

	out, err = runCmd(t, "entities", "apply", "--database-url", url, "--roster", rosterPath)
	require.NoError(t, err)
	require.Contains(t, out, "# inserted 1 rows")

	out, err = runCmd(t, "entities", "check", "--database-url", url)
	require.NoError(t, err,
		"the link is in the map now, so the finding is closed; a monthly item that stays red after the fix is one nobody reads")
	require.Contains(t, out, "# 0 accepted candidate(s) the map does not carry")
	require.NotContains(t, out, "UNLINKED")
}

// TestEntitiesCheckCommand_SkipCandidatesRunsCleanAndSaysWhatItDidNotCheck
// pins the flag that can turn the control off. It exits zero on the very
// store the test above fails on, so the line naming what the run did not look
// at is the only thing standing between "--skip-candidates in the cron entry"
// and a green monthly item over a store missing every link.
func TestEntitiesCheckCommand_SkipCandidatesRunsCleanAndSaysWhatItDidNotCheck(t *testing.T) {
	url := seedCheckStore(t)

	out, err := runCmd(t, "entities", "check", "--database-url", url, "--skip-candidates")
	require.NoError(t, err)
	require.Contains(t, out, "# 0 violations")
	require.Contains(t, out, "this run says nothing about successions the map is missing")
	require.NotContains(t, out, "UNLINKED")
}
