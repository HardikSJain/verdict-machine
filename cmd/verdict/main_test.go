package main

import (
	"bytes"
	"testing"

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
