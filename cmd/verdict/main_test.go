package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestVersionCommandPrintsVersion(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	require.NoError(t, root.Execute())
	require.Equal(t, "verdict 0.0.0-dev\n", out.String())
}

// cmdWithDatabaseURLFlag builds a bare *cobra.Command carrying the same
// local "database-url" flag newMigrateCmd declares, so databaseURL's
// resolution logic can be exercised directly without running a command.
func cmdWithDatabaseURLFlag() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	return cmd
}

func TestDatabaseURL_FlagWinsOverEnv(t *testing.T) {
	t.Setenv("VERDICT_DATABASE_URL", "postgres://from-env")
	cmd := cmdWithDatabaseURLFlag()
	require.NoError(t, cmd.Flags().Set("database-url", "postgres://from-flag"))

	url, err := databaseURL(cmd)
	require.NoError(t, err)
	require.Equal(t, "postgres://from-flag", url)
}

func TestDatabaseURL_EnvUsedWhenFlagAbsent(t *testing.T) {
	t.Setenv("VERDICT_DATABASE_URL", "postgres://from-env")
	cmd := cmdWithDatabaseURLFlag()

	url, err := databaseURL(cmd)
	require.NoError(t, err)
	require.Equal(t, "postgres://from-env", url)
}

func TestDatabaseURL_MissingReturnsExactError(t *testing.T) {
	t.Setenv("VERDICT_DATABASE_URL", "")
	cmd := cmdWithDatabaseURLFlag()

	_, err := databaseURL(cmd)
	require.EqualError(t, err, "set --database-url or VERDICT_DATABASE_URL")
}

// TestDatabaseURL_FallsThroughToInheritedFlag exercises the case a future
// command will hit: "database-url" declared as a persistent flag on a
// parent command rather than locally. Before the fix, the local
// cmd.Flags().GetString call's "flag accessed but not defined" error was
// silently discarded via `url, _ := ...`, which happened to still work only
// because the zero value ("") looked the same as "flag genuinely unset" --
// this test pins the fallthrough to the inherited flag explicitly.
func TestDatabaseURL_FallsThroughToInheritedFlag(t *testing.T) {
	t.Setenv("VERDICT_DATABASE_URL", "")
	root := &cobra.Command{Use: "root"}
	root.PersistentFlags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	require.NoError(t, root.PersistentFlags().Set("database-url", "postgres://from-parent"))
	child := &cobra.Command{Use: "child"}
	root.AddCommand(child)

	url, err := databaseURL(child)
	require.NoError(t, err)
	require.Equal(t, "postgres://from-parent", url)
}

func TestMigrateCommand_DeclaresDatabaseURLFlag(t *testing.T) {
	cmd := newMigrateCmd()
	f := cmd.Flags().Lookup("database-url")
	require.NotNil(t, f)
	require.Equal(t, "", f.DefValue)
}

// TestMigrateCommand_MissingDatabaseURLReturnsError exercises newMigrateCmd's
// RunE end to end through root.Execute(), but only up to the point where it
// resolves --database-url: with neither the flag nor VERDICT_DATABASE_URL
// set, RunE must return databaseURL's error before ever calling db.Migrate,
// so this never touches a real database.
func TestMigrateCommand_MissingDatabaseURLReturnsError(t *testing.T) {
	t.Setenv("VERDICT_DATABASE_URL", "")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"migrate"})

	err := root.Execute()
	require.EqualError(t, err, "set --database-url or VERDICT_DATABASE_URL")
}

// TestBackfillCommand_MissingDatabaseURLReturnsError mirrors
// TestMigrateCommand_MissingDatabaseURLReturnsError: newBackfillCmd's RunE
// has the identical shape (parse flags, then resolve database URL, then
// connect), and must return databaseURL's error before ever calling
// db.Connect, so this never touches a real database or the network.
func TestBackfillCommand_MissingDatabaseURLReturnsError(t *testing.T) {
	t.Setenv("VERDICT_DATABASE_URL", "")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"backfill"})

	err := root.Execute()
	require.EqualError(t, err, "set --database-url or VERDICT_DATABASE_URL")
}

// TestParseIngestPin covers the three forms `entities check --as-of-ingest`
// accepts and the one it does not. The flag exists because migration 0004's
// flatness trigger only guarantees the map is flat as resolved at now(), so
// the map a past run actually read has to be checkable after the fact --
// which means the pin has to survive being typed.
func TestParseIngestPin(t *testing.T) {
	before := time.Now()
	got, err := parseIngestPin("")
	require.NoError(t, err)
	require.False(t, got.Before(before), "an empty pin is now(), which is what the command did before the flag existed")

	got, err = parseIngestPin("2026-09-07T18:30:00+05:30")
	require.NoError(t, err)
	require.True(t, got.Equal(time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)),
		"an RFC3339 pin keeps its offset; this is the form a recorded run hands back")

	got, err = parseIngestPin("2026-09-07")
	require.NoError(t, err)
	require.True(t, got.Equal(time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)),
		"a bare date is midnight UTC and not midnight local: a pin whose instant depends on where the operator sits is not a pin")

	_, err = parseIngestPin("last tuesday")
	require.Error(t, err)
	require.ErrorContains(t, err, "last tuesday", "the error has to quote what was typed")
	require.ErrorContains(t, err, "RFC3339", "and say what would have worked")
}

// TestEntitiesCheckCommand_RejectsABadAsOfIngestBeforeTouchingTheDatabase
// pins the ORDER of the two failures, not just that both exist. With no
// database URL set, a bad --as-of-ingest must still be reported as a bad
// --as-of-ingest: parsing it after resolving the URL would tell an operator
// their connection string is missing when what is actually wrong is the
// timestamp they typed, and would also mean a mistyped pin is only caught
// after a connection is opened.
func TestEntitiesCheckCommand_RejectsABadAsOfIngestBeforeTouchingTheDatabase(t *testing.T) {
	t.Setenv("VERDICT_DATABASE_URL", "")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"entities", "check", "--as-of-ingest", "last tuesday"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "--as-of-ingest")
	require.NotContains(t, err.Error(), "VERDICT_DATABASE_URL",
		"the pin is parsed first, so this never gets as far as needing a database")
}

// TestEntitiesCheckCommand_DeclaresAsOfIngestFlag pins the default: absent,
// the command checks the map at now(), which is what it did before the flag
// existed.
func TestEntitiesCheckCommand_DeclaresAsOfIngestFlag(t *testing.T) {
	entities := newEntitiesCmd()
	var check *cobra.Command
	for _, c := range entities.Commands() {
		if c.Name() == "check" {
			check = c
		}
	}
	require.NotNil(t, check)
	f := check.Flags().Lookup("as-of-ingest")
	require.NotNil(t, f)
	require.Equal(t, "", f.DefValue)
}

// TestEntitiesCheckCommand_MissingDatabaseURLReturnsError mirrors the migrate
// and backfill cases: `entities check` takes --database-url from a persistent
// flag on its parent, which is the inherited-flag path databaseURL exists to
// handle, so this also pins that the grouping command wired it up.
func TestEntitiesCheckCommand_MissingDatabaseURLReturnsError(t *testing.T) {
	t.Setenv("VERDICT_DATABASE_URL", "")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"entities", "check"})

	err := root.Execute()
	require.EqualError(t, err, "set --database-url or VERDICT_DATABASE_URL")
}
