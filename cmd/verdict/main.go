// Command verdict is the Verdict Machine binary: ingest, backtest, evening, replay, verify.
package main

import (
	"context"
	"fmt"
	"os"

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

// databaseURL resolves the connection string from the flag, then the environment.
func databaseURL(cmd *cobra.Command) (string, error) {
	url, _ := cmd.Flags().GetString("database-url")
	if url == "" {
		url = os.Getenv("VERDICT_DATABASE_URL")
	}
	if url == "" {
		return "", fmt.Errorf("set --database-url or VERDICT_DATABASE_URL")
	}
	return url, nil
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
