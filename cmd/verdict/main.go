// Command verdict is the Verdict Machine binary: ingest, backtest, evening, replay, verify.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/HardikSJain/verdict-machine/internal/db"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "verdict",
		Short:         "Verdict Machine: a daily-bar NSE strategy lab that earns autonomy",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newMigrateCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "verdict "+version)
			return err
		},
	}
}

// databaseURL resolves the connection string from the local flag, then an
// inherited flag (for a command that gets --database-url from a parent's
// persistent flags rather than declaring it itself), then the environment.
func databaseURL(cmd *cobra.Command) (string, error) {
	url, err := lookupFlagString(cmd.Flags(), "database-url")
	if err != nil {
		return "", err
	}
	if url == "" {
		url, err = lookupFlagString(cmd.InheritedFlags(), "database-url")
		if err != nil {
			return "", err
		}
	}
	if url == "" {
		url = os.Getenv("VERDICT_DATABASE_URL")
	}
	if url == "" {
		return "", fmt.Errorf("set --database-url or VERDICT_DATABASE_URL")
	}
	return url, nil
}

// stringFlagGetter is satisfied by *pflag.FlagSet, as returned by both
// cmd.Flags() and cmd.InheritedFlags(); declared locally so this file does
// not need to import pflag directly just to name the parameter type.
type stringFlagGetter interface {
	GetString(name string) (string, error)
}

// lookupFlagString reads a string flag. pflag's GetString returns an error
// of "flag accessed but not defined: <name>" when the flag was never
// declared on that FlagSet -- expected when a flag lives on a different
// command in the tree (e.g. local here, inherited there), so that case is
// treated as "no value" and the caller falls through to the next source.
// Any other error means the flag was declared with the wrong type, which is
// a programming error, and is wrapped and returned instead of being
// silently discarded.
func lookupFlagString(flags stringFlagGetter, name string) (string, error) {
	val, err := flags.GetString(name)
	if err != nil {
		if strings.Contains(err.Error(), "flag accessed but not defined") {
			return "", nil
		}
		return "", fmt.Errorf("%s flag: %w", name, err)
	}
	return val, nil
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			url, err := databaseURL(cmd)
			if err != nil {
				return err
			}
			if err := db.Migrate(cmd.Context(), url); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "migrations applied")
			return err
		},
	}
	cmd.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	return cmd
}

func main() {
	if err := newRootCmd().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
