package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
		{"propose", []string{"out", "review-out", "as-of-ingest", "ratified-by"}},
		{"link", []string{"pred", "succ", "note", "ratified-by", "roster"}},
		{"apply", []string{"roster", "dry-run", "seeded-by"}},
		{"retract", []string{"entity", "symbol", "note"}},
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
