// Command verdict is the Verdict Machine binary: ingest, backtest, evening, replay, verify.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/HardikSJain/verdict-machine/internal/db"
	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/bhavcopy"
	"github.com/HardikSJain/verdict-machine/internal/market/eod2"
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
	root.AddCommand(newIngestCmd())
	root.AddCommand(newBackfillCmd())
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

func newIngestCmd() *cobra.Command {
	ingest := &cobra.Command{Use: "ingest", Short: "Load market data into the store"}
	ingest.PersistentFlags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")

	e := &cobra.Command{
		Use:   "eod2",
		Short: "Load eod2's adjusted daily CSVs (source=eod2)",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			if dir == "" {
				return fmt.Errorf("--dir is required (the eod2_data directory)")
			}
			url, err := databaseURL(cmd)
			if err != nil {
				return err
			}
			pool, err := db.Connect(cmd.Context(), url)
			if err != nil {
				return err
			}
			defer pool.Close()

			sym2isin, err := eod2.LoadSymbolMap(filepath.Join(dir, "isin_symbol_map.json"))
			if err != nil {
				return err
			}
			bars, skipped, err := eod2.LoadDir(filepath.Join(dir, "daily"), sym2isin)
			if err != nil {
				return err
			}
			started := time.Now()
			n, err := market.NewStore(pool).InsertBars(cmd.Context(), market.SourceEod2, bars)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "eod2: parsed %d bars, inserted %d new versions, skipped %d tickers without ISIN, %s\n",
				len(bars), n, len(skipped), time.Since(started).Round(time.Millisecond))
			return err
		},
	}
	e.Flags().String("dir", "", "path to eod2_data (contains daily/ and isin_symbol_map.json)")
	ingest.AddCommand(e)
	return ingest
}

func newBackfillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Download NSE bhavcopy archives into the store (source=nse-bhavcopy); resumable",
		RunE: func(cmd *cobra.Command, args []string) error {
			fromS, _ := cmd.Flags().GetString("from")
			toS, _ := cmd.Flags().GetString("to")
			delay, _ := cmd.Flags().GetDuration("delay")
			from, err := time.Parse("2006-01-02", fromS)
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			to := time.Now().UTC()
			if toS != "" {
				if to, err = time.Parse("2006-01-02", toS); err != nil {
					return fmt.Errorf("--to: %w", err)
				}
			}
			url, err := databaseURL(cmd)
			if err != nil {
				return err
			}
			pool, err := db.Connect(cmd.Context(), url)
			if err != nil {
				return err
			}
			defer pool.Close()
			sum, err := bhavcopy.Backfill(cmd.Context(), market.NewStore(pool), bhavcopy.NewFetcher(), from, to, delay, cmd.ErrOrStderr())
			fmt.Fprintf(cmd.OutOrStdout(), "backfill: fetched %d, no-file %d, errors %d, skipped %d, inserted %d bars\n",
				sum.Fetched, sum.NoFile, sum.Errors, sum.Skipped, sum.Inserted)
			return err
		},
	}
	cmd.Flags().String("from", "2011-09-01", "first session date (YYYY-MM-DD)")
	cmd.Flags().String("to", "", "last session date (default today)")
	cmd.Flags().Duration("delay", 750*time.Millisecond, "pause between requests")
	cmd.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	return cmd
}

func main() {
	if err := newRootCmd().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
