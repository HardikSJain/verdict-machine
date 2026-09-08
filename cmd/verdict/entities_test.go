package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market/entities"
)

// The refusals that must happen before a database connection is opened. Each
// one is a mistake that would otherwise be reported as something else -- a
// connection error, or worse, a successful-looking no-op.

func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestEntitiesRetract_RequiresExactlyOneOfEntityOrSymbol(t *testing.T) {
	// Naming neither is an operator who has not decided what they mean.
	// Naming both is worse: --entity dissolves an entity and --symbol detaches
	// one member, and silently preferring one would answer a question the
	// operator did not ask.
	_, err := runCmd(t, "entities", "retract", "--database-url", "postgres://unused/x")
	require.ErrorContains(t, err, "exactly one of --entity or --symbol")

	_, err = runCmd(t, "entities", "retract", "--entity", "3", "--symbol", "3", "--database-url", "postgres://unused/x")
	require.ErrorContains(t, err, "exactly one of --entity or --symbol")
}

func TestEntitiesLink_RequiresANoteAndARatifier(t *testing.T) {
	// A hand-written line is the highest-risk row the store can hold: it is
	// the one no gate admitted. Design 5.6 requires an external NSE
	// corporate-action source behind it, and the note is where that goes.
	_, err := runCmd(t, "entities", "link",
		"--pred", "INF732E01011", "--succ", "INF204KB14I2", "--database-url", "postgres://unused/x")
	require.ErrorContains(t, err, "--note and --ratified-by are required")

	_, err = runCmd(t, "entities", "link", "--database-url", "postgres://unused/x")
	require.ErrorContains(t, err, "--pred and --succ are required")
}

func TestEntitiesApply_ValidatesTheRosterBeforeTouchingTheDatabase(t *testing.T) {
	// The gates run against the file on disk every time rows are written, so
	// a bad roster fails as a bad roster rather than as a database error.
	out, err := runCmd(t, "entities", "apply", "--roster", "testdata/does-not-exist.json",
		"--database-url", "postgres://unused/x")
	require.Error(t, err)
	require.NotContains(t, strings.ToLower(err.Error()), "connect",
		"the roster is read first; a missing file must not be reported as a connection problem")
	require.Empty(t, out)
}

func TestEntitiesPropose_RejectsABadIngestPinBeforeTheDatabaseURL(t *testing.T) {
	_, err := runCmd(t, "entities", "propose", "--as-of-ingest", "yesterday")
	require.ErrorContains(t, err, "--as-of-ingest")
	require.ErrorContains(t, err, "RFC3339")
}

func TestEntitiesCommandsDeclareTheirFlags(t *testing.T) {
	for _, tc := range []struct {
		cmd   string
		flags []string
	}{
		{"propose", []string{"out", "review-out", "as-of-ingest", "ratified-by", "discard-manual"}},
		{"link", []string{"pred", "succ", "note", "ratified-by", "roster"}},
		{"apply", []string{"roster", "dry-run", "seeded-by"}},
		{"retract", []string{"entity", "symbol", "note"}},
		// check grew --skip-candidates in stage 3: the candidate scan is on
		// by default, because a monthly item that has to be asked for is one
		// that gets run without it and reports "no violations" from a store
		// that is missing four hundred links.
		{"check", []string{"as-of-ingest", "skip-candidates"}},
	} {
		root := newRootCmd()
		entities, _, err := root.Find([]string{"entities", tc.cmd})
		require.NoError(t, err)
		require.Equal(t, tc.cmd, entities.Name())
		for _, f := range tc.flags {
			require.NotNilf(t, entities.Flags().Lookup(f), "entities %s --%s", tc.cmd, f)
		}
		require.NotNil(t, entities.InheritedFlags().Lookup("database-url"))
	}
}

// ratifiedAt dates a ratification only when there is a ratifier to date: a
// generated file is not ratified by having been generated, and a date beside
// an empty name would read as though someone had signed it.
func TestRatifiedAtIsEmptyWithoutARatifier(t *testing.T) {
	require.Empty(t, ratifiedAt(""))
	require.NotEmpty(t, ratifiedAt("hardik"))
}

// TestProposeReadsTheManualLinesOutOfTheFileItIsAboutToOverwrite covers the
// half of the manual-line rescue that lives in the CLI: the read happens
// before a connection is opened, so a roster carrying a human's decision
// cannot be lost to a run that then fails on the database.
func TestProposeReadsTheManualLinesOutOfTheFileItIsAboutToOverwrite(t *testing.T) {
	dir := t.TempDir()

	// No file yet: the first ever run has nothing to carry and must not fail.
	got, err := existingManualLines(filepath.Join(dir, "absent.json"), false)
	require.NoError(t, err)
	require.Empty(t, got)

	// A roster with one hand-written line and one generated one: only the
	// hand-written line comes back.
	path := filepath.Join(dir, "roster.json")
	r := &entities.Roster{Version: 1, Links: []entities.Link{manualCLILine(), generatedCLILine()}}
	b, err := r.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o644))

	got, err = existingManualLines(path, false)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "INF204KB14I2", got[0].Successor)

	// A file that will not load is not a file to overwrite quietly: it may be
	// mid-edit, and the only copy of a human decision is in it.
	require.NoError(t, os.WriteFile(path, []byte("{\"version\": 1, \"links\": ["), 0o644))
	_, err = existingManualLines(path, false)
	require.Error(t, err)
	require.ErrorContains(t, err, "--discard-manual")

	// And --discard-manual is the way past it, in both directions.
	got, err = existingManualLines(path, true)
	require.NoError(t, err)
	require.Empty(t, got)
}

func manualCLILine() entities.Link {
	return entities.Link{
		Reason:           entities.ReasonManual,
		Predecessor:      "INF732E01011",
		Successor:        "INF204KB14I2",
		TickerAtBoundary: "NIFTYBEES",
		EffectiveFrom:    "2019-12-20",
		Gates: entities.Gates{
			CheckDigit:         true,
			PredecessorLastBar: "2019-12-19",
			SuccessorFirstBar:  "2019-12-20",
		},
		RatifiedBy: "hardik", RatifiedAt: "2026-09-08",
		Note: "NSE circular: AMC transfer",
	}
}

func generatedCLILine() entities.Link {
	return entities.Link{
		Reason:           entities.ReasonSuccession,
		Predecessor:      "INE081A01012",
		Successor:        "INE081A01020",
		TickerAtBoundary: "TATASTEEL",
		EffectiveFrom:    "2022-07-29",
		Gates: entities.Gates{
			CheckDigit:           true,
			IssuerPrefix:         "INE081A01",
			Serial:               "01->02",
			PredecessorLastBar:   "2022-07-28",
			SuccessorFirstBar:    "2022-07-29",
			CalendarDaysBetween:  1,
			GapDatesAllSettled:   true,
			BoundaryCloseRatio:   1.0723,
			BoundaryRatioMatches: "1/1",
		},
	}
}
