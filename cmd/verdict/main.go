// Command verdict is the Verdict Machine binary: ingest, backtest, evening, replay, verify.
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/HardikSJain/verdict-machine/internal/cost"
	"github.com/HardikSJain/verdict-machine/internal/db"
	"github.com/HardikSJain/verdict-machine/internal/engine"
	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/bhavcopy"
	"github.com/HardikSJain/verdict-machine/internal/market/entities"
	"github.com/HardikSJain/verdict-machine/internal/market/eod2"
	"github.com/HardikSJain/verdict-machine/internal/market/nseindex"
	"github.com/HardikSJain/verdict-machine/internal/report"
	"github.com/HardikSJain/verdict-machine/internal/risk"
	"github.com/HardikSJain/verdict-machine/internal/strategy"
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
	root.AddCommand(newUniverseCmd())
	root.AddCommand(newEntitiesCmd())
	root.AddCommand(newIndexCmd())
	root.AddCommand(newBacktestCmd())
	root.AddCommand(newAdjustmentsCmd())
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

func newUniverseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "universe",
		Short: "Print the point-in-time universe: top N by median rupee turnover",
		RunE: func(cmd *cobra.Command, args []string) error {
			asOfS, _ := cmd.Flags().GetString("as-of")
			lookback, _ := cmd.Flags().GetInt("lookback")
			n, _ := cmd.Flags().GetInt("n")
			source, _ := cmd.Flags().GetString("source")
			allowStale, _ := cmd.Flags().GetBool("allow-stale")
			asOf, err := time.Parse("2006-01-02", asOfS)
			if err != nil {
				return fmt.Errorf("--as-of: %w", err)
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
			var opts []market.UniverseOption
			if eqOnly, _ := cmd.Flags().GetBool("equities-only"); eqOnly {
				opts = append(opts, market.EquitiesOnly())
			}
			if allowStale {
				opts = append(opts, market.AllowStaleMembers())
			}
			u, err := market.NewStore(pool).UniverseAsOf(cmd.Context(), source, asOf, lookback, n, time.Now(), opts...)
			if err != nil {
				return err
			}
			writeUniverse(cmd.OutOrStdout(), u, asOf, source, lookback)
			return nil
		},
	}
	cmd.Flags().String("as-of", time.Now().UTC().Format("2006-01-02"), "session date (YYYY-MM-DD)")
	cmd.Flags().Int("lookback", 125, "sessions in the ranking window (about six months)")
	cmd.Flags().Int("n", 500, "universe size")
	cmd.Flags().String("source", market.SourceBhavcopy, "bar source: nse-bhavcopy or eod2")
	cmd.Flags().Bool("equities-only", false, "drop fund units (INF/IN9 ISINs)")
	cmd.Flags().Bool("allow-stale", false, "keep names whose last bar predates the window's final session (halted/suspended names); default drops them")
	cmd.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	return cmd
}

// writeUniverse renders one universe, and it is a separate function so the
// header can be asserted on without a database.
//
// The last_break column is the fence. After the entity change a company that
// changed ISIN comes back as ONE member with a continuous 125-session
// window, which is the whole point -- and is exactly what makes the prices
// more dangerous than they were: nse-bhavcopy is unadjusted and no
// adjustments layer exists, so the series is continuous in identity and
// discontinuous in LEVEL. Before this change the two halves were separate
// members and nothing could difference across the split by accident. Now a
// 12-1 momentum reading across a 1:10 split returns -90% and looks like a
// number. The factor is not recoverable from the boundary either: at the
// TATASTEEL boundary the ex-split session falls one session BEFORE the ISIN
// changes, so the boundary ratio reads 100.35 -> 107.60 and says "no split".
// So the column names the date and the header says what it costs.
//
// The count line prints even at zero. The live store's map is empty until a
// human applies the roster, and "0 of N" is the operator's evidence that the
// map was consulted and came back empty -- a different statement from the
// fence not being wired at all.
func writeUniverse(w io.Writer, u market.Universe, asOf time.Time, source string, lookback int) {
	// liveness names which membership rule actually ran, because
	// RequiredLastSession is the only thing distinguishing "no halted
	// names qualified" from "--allow-stale was silently ignored".
	liveness := "last-session-required"
	if !u.RequiredLastSession {
		liveness = "allow-stale"
	}
	// The realised window comes first: a store that holds fewer than
	// --lookback sessions ranks on what it has, and the rows below
	// look exactly the same either way.
	fmt.Fprintf(w, "# as-of %s, source %s, window %d of %d sessions requested, %d symbols, liveness=%s\n",
		asOf.Format("2006-01-02"), source, u.Sessions, lookback, len(u.Members), liveness)
	broken := 0
	for _, m := range u.Members {
		if m.LastBreak != nil {
			broken++
		}
	}
	fmt.Fprintf(w, "# succession: %d of %d members carry a succession boundary at or before as-of\n",
		broken, len(u.Members))
	if broken > 0 {
		fmt.Fprintln(w, "# a return computed across last_break is wrong by the split factor: nse-bhavcopy prices are"+
			" unadjusted and the read-time adjustments layer does not exist. Call EntityBoundaries and refuse the return.")
	}
	fmt.Fprintf(w, "rank\tticker\tisin\tmedian_turnover_inr\tdays\tlast_break\n")
	for i, m := range u.Members {
		// A dash, not an empty cell: a blank field in a tab-separated row
		// reads as a parse error rather than as "no boundary".
		lastBreak := "-"
		if m.LastBreak != nil {
			lastBreak = m.LastBreak.Format("2006-01-02")
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%.0f\t%d\t%s\n",
			i+1, m.Ticker, m.ISIN, m.MedianTurnover, m.DaysPresent, lastBreak)
	}
}

// parseIngestPin turns `--as-of-ingest` into the ingested_at pin every store
// read takes. An empty value means now(), which is what the command did
// before the flag existed and stays the default.
//
// Two forms are accepted because two different people type this. An RFC3339
// timestamp is what a recorded run hands back, and it is the form that can
// name a moment precisely enough to replay one. A bare YYYY-MM-DD is what a
// human types, and it is read as midnight UTC rather than midnight local: a
// pin that means a different instant depending on where the operator is
// sitting is not a pin, and ingested_at is timestamptz.
func parseIngestPin(s string) (time.Time, error) {
	if s == "" {
		return time.Now(), nil
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts, nil
	}
	ts, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"want an RFC3339 timestamp (2026-09-07T18:30:00+05:30) or a date (2026-09-07, read as midnight UTC), got %q", s)
	}
	return ts, nil
}

// newEntitiesCmd groups the commands that operate on the canonical-entity
// overlay: `check` reads the map, `propose` proposes one, `link` edits the
// file a human reviews, `apply` writes the rows and `retract` unwrites them.
//
// The split between propose and apply is the safety argument, not a
// convenience. The decision that two ISINs are one company is made in a
// reviewed file and not by a command, so nothing here runs automatically
// after an ingest and nothing writes a link the roster does not contain.
func newEntitiesCmd() *cobra.Command {
	entities := &cobra.Command{
		Use:   "entities",
		Short: "Inspect the canonical-entity overlay (symbol_links)",
	}
	entities.PersistentFlags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")

	check := &cobra.Command{
		Use:   "check",
		Short: "Run the entity invariants and report succession candidates the map does not carry",
		Long: "Run the entity invariants against the current map, then look for successions\n" +
			"the map has not been told about. This is the monthly ops item.\n\n" +
			"I1 (flatness) and I2 (disjointness) are defects and exit non-zero. I3 (issuer\n" +
			"agreement) is advisory and is printed without failing the command: a face-value\n" +
			"split keeps the NSDL issuer code, so a mismatch is a question for a human rather\n" +
			"than a proven error.\n\n" +
			"The candidate scan re-runs the generator read-only and reports every pair the\n" +
			"gates would accept that the map does not already carry. An accepted candidate\n" +
			"missing from the map exits non-zero: NSE reissues an ISIN 30 to 40 times a year\n" +
			"and each one drops a name out of the point-in-time universe around its boundary\n" +
			"until a roster PR is merged and applied. Quarantined pairs are printed as a\n" +
			"count and do not fail the command -- that queue is 181 deep and is not going to\n" +
			"be worked. The scan inherits G4a, so it also exits non-zero when the archive\n" +
			"holds a hole -- an ingest_log date left unlogged or 'error' after the last fetch\n" +
			"had the chance to settle it. With a hole in the archive a demerger reads as\n" +
			"adjacent and 'no new candidates' would be a false negative. The recent unlogged\n" +
			"tail backfill leaves on purpose is pending rather than a hole and does not stop\n" +
			"the run; a candidate whose own gap dates are unsettled is quarantined anyway.\n" +
			"--skip-candidates runs the invariants alone, which is the right thing mid-backfill\n" +
			"and on a store with no bars.\n\n" +
			"The same scan re-runs the gates against the links the map ALREADY holds and\n" +
			"prints a STALE line for every merged pair the gates no longer accept. Design 5.5's\n" +
			"\"every gate is independently re-checkable\" is this leg, and nothing else in the\n" +
			"system can say that a standing merge stopped passing its own gates. It is\n" +
			"reported and does not fail the command: an applied hand-written line fails a gate\n" +
			"by construction -- an INF fund-unit transfer fails G0 and G1 -- so failing on one\n" +
			"would make the monthly item permanently red.\n\n" +
			"What this cannot do: a wrong-but-DISJOINT merge -- a reverse-merger shell, a\n" +
			"freed ticker reused by a different company -- is undetectable from inside the\n" +
			"store, and no exit code here should be read as saying otherwise.\n\n" +
			"--as-of-ingest evaluates the invariants against the map as the store knew it at\n" +
			"a past moment rather than at now(). It is not a convenience. Migration 0004's\n" +
			"flatness trigger reads the map as resolved at now(), so what it guarantees is\n" +
			"that the CURRENT map is flat -- a row inserted with a backdated ingested_at can\n" +
			"leave a past pin resolving through two hops while the present map looks clean.\n" +
			"CheckEntityInvariants has always been pinned; this flag is what makes the pin\n" +
			"reachable, so the map a recorded run actually read can be checked after the\n" +
			"fact rather than only the map today's readers see.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parsed before the database URL is resolved, so a mistyped pin
			// is reported as a mistyped pin rather than as a missing URL.
			asOfIngestS, _ := cmd.Flags().GetString("as-of-ingest")
			skipCandidates, _ := cmd.Flags().GetBool("skip-candidates")
			asOfIngest, err := parseIngestPin(asOfIngestS)
			if err != nil {
				return fmt.Errorf("--as-of-ingest: %w", err)
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

			store := market.NewStore(pool)
			violations, err := store.CheckEntityInvariants(cmd.Context(), asOfIngest)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			var fatal int
			for _, v := range violations {
				severity := "VIOLATION"
				if v.Advisory() {
					severity = "advisory"
				} else {
					fatal++
				}
				fmt.Fprintf(w, "%s\t%s\tentity %d\t%s\n", severity, v.Kind, v.EntityID, v.Detail)
			}
			// The pin is printed, always. An operator reading a clean report
			// has to be able to tell WHICH map came back clean.
			fmt.Fprintf(w, "# %d violations, %d advisory, map as of ingest %s\n",
				fatal, len(violations)-fatal, asOfIngest.Format(time.RFC3339))

			// The invariant report is printed BEFORE the candidate scan runs
			// and before it can fail. The scan takes about five seconds and
			// refuses outright on an unsettled archive; neither is a reason
			// to withhold an answer about the map that was already computed.
			gaps := 0
			if !skipCandidates {
				if gaps, err = scanCandidates(cmd.Context(), pool, store, asOfIngest, w); err != nil {
					return fmt.Errorf("entities check: %w", err)
				}
			} else {
				fmt.Fprintln(w, "# candidate scan skipped (--skip-candidates): this run says nothing about successions the map is missing,"+
					" and nothing about whether the links it already holds still pass their gates")
			}

			switch {
			case fatal > 0 && gaps > 0:
				return fmt.Errorf("entities check: %d entity invariant violations, and %d accepted candidate(s) the map does not carry", fatal, gaps)
			case fatal > 0:
				return fmt.Errorf("entities check: %d entity invariant violations", fatal)
			case gaps > 0:
				return fmt.Errorf("entities check: %d accepted candidate(s) the map does not carry; regenerate the roster with `verdict entities propose`, review the diff, then apply it", gaps)
			}
			return nil
		},
	}
	check.Flags().String("as-of-ingest", "",
		"evaluate the map as the store knew it at this ingest timestamp: RFC3339, or YYYY-MM-DD for midnight UTC (default now)")
	check.Flags().Bool("skip-candidates", false,
		"run the invariants only, without re-generating succession candidates (use mid-backfill, or on a store with no bars)")
	entities.AddCommand(check)
	entities.AddCommand(newEntitiesProposeCmd())
	entities.AddCommand(newEntitiesLinkCmd())
	entities.AddCommand(newEntitiesApplyCmd())
	entities.AddCommand(newEntitiesRetractCmd())
	return entities
}

func main() {
	if err := newRootCmd().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newIndexCmd() *cobra.Command {
	index := &cobra.Command{
		Use:   "index",
		Short: "NSE daily index archive: backfill and identity checks",
	}
	index.AddCommand(newIndexBackfillCmd())
	index.AddCommand(newIndexCheckCmd())
	return index
}

func newIndexBackfillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Download NSE index archives into index_levels (source=nse-index); resumable",
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
			sum, err := nseindex.Backfill(cmd.Context(), market.NewStore(pool), nseindex.NewFetcher(),
				from, to, delay, cmd.ErrOrStderr())
			fmt.Fprintf(cmd.OutOrStdout(),
				"index backfill: fetched %d, no-file %d, errors %d, skipped %d, inserted %d levels\n",
				sum.Fetched, sum.NoFile, sum.Errors, sum.Skipped, sum.Inserted)
			return err
		},
	}
	cmd.Flags().String("from", "2012-02-01", "first session date (YYYY-MM-DD)")
	cmd.Flags().String("to", "", "last session date (default today)")
	cmd.Flags().Duration("delay", 750*time.Millisecond, "pause between requests")
	cmd.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	return cmd
}

// newIndexCheckCmd verifies the alias table against the data it claims to
// describe. A rebrand must leave the level alone; a name change that comes with
// a rebasing is a different index wearing an old name and must not be merged.
func newIndexCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify every curated index alias: continuity across each rename, and gaps",
		RunE: func(cmd *cobra.Command, args []string) error {
			tol, _ := cmd.Flags().GetFloat64("tolerance")
			url, err := databaseURL(cmd)
			if err != nil {
				return err
			}
			pool, err := db.Connect(cmd.Context(), url)
			if err != nil {
				return err
			}
			defer pool.Close()
			store := market.NewStore(pool)
			pin := time.Now()
			out := cmd.OutOrStdout()
			var failures int

			for _, code := range nseindex.CuratedCodes() {
				names, err := nseindex.NamesFor(code)
				if err != nil {
					return err
				}
				changes, err := store.NameChanges(cmd.Context(), code, pin)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "%s  (%s)\n", code, strings.Join(names, " -> "))
				if len(changes) == 0 {
					fmt.Fprintf(out, "  no rename observed in the store\n")
					continue
				}
				for _, c := range changes {
					jump := c.Close/c.PrevClose - 1
					status := "ok"
					switch {
					case c.GapDays > 5:
						// Across a hole in the archive a rebasing and an
						// ordinary market move are indistinguishable. Reporting
						// it as either would be a guess.
						status = fmt.Sprintf("UNVERIFIABLE (%d-day gap)", c.GapDays)
						failures++
					case jump > tol || jump < -tol:
						status = "REBASED?"
						failures++
					}
					fmt.Fprintf(out, "  %s %-18s %10.2f  ->  %s %-18s %10.2f   %+.2f%%  %s\n",
						c.PrevDate.Format(time.DateOnly), c.FromName, c.PrevClose,
						c.Date.Format(time.DateOnly), c.ToName, c.Close, 100*jump, status)
				}
				gaps, err := store.Gaps(cmd.Context(), code, 10, pin)
				if err != nil {
					return err
				}
				for i, g := range gaps {
					if i >= 3 {
						fmt.Fprintf(out, "  ... and %d more gaps over 10 days\n", len(gaps)-3)
						break
					}
					fmt.Fprintf(out, "  GAP %d days: absent between %s and %s\n",
						g.GapDays, g.After.Format(time.DateOnly), g.Before.Format(time.DateOnly))
				}
			}
			if failures > 0 {
				fmt.Fprintf(out, "\n%d rename(s) could not be shown continuous within %.1f%%: a rebasing is a different index wearing an old name and must not share a code\n",
					failures, 100*tol)
				return fmt.Errorf("index check: %d suspect alias(es)", failures)
			}
			fmt.Fprintf(out, "\nall curated aliases continuous within %.1f%%\n", 100*tol)
			return nil
		},
	}
	cmd.Flags().Float64("tolerance", 0.06,
		"maximum level change across a rename before it is treated as a rebasing")
	cmd.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	return cmd
}

// gitSHA is the commit the binary was built from. Go stamps it into build info
// for a `go build` from a repository; VERDICT_GIT_SHA overrides it for a `go
// run`, where the stamp is absent. A run recorded without it is still a run, but
// its code cannot be recovered, so the value says so rather than being blank.
func mustBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}

func gitSHA() string {
	if v := os.Getenv("VERDICT_GIT_SHA"); v != "" {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				if dirty := vcsModified(info); dirty {
					return s.Value + "-dirty"
				}
				return s.Value
			}
		}
	}
	return "unknown"
}

func vcsModified(info *debug.BuildInfo) bool {
	for _, s := range info.Settings {
		if s.Key == "vcs.modified" {
			return s.Value == "true"
		}
	}
	return false
}

func newBacktestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backtest",
		Short: "Run a strategy over history and print the report (see docs/experiments/)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fromS, _ := cmd.Flags().GetString("from")
			toS, _ := cmd.Flags().GetString("to")
			capital, _ := cmd.Flags().GetFloat64("capital")
			top, _ := cmd.Flags().GetInt("top")
			formation, _ := cmd.Flags().GetInt("formation")
			skip, _ := cmd.Flags().GetInt("skip")
			lookback, _ := cmd.Flags().GetInt("lookback")
			n, _ := cmd.Flags().GetInt("n")
			source, _ := cmd.Flags().GetString("source")
			useFilter, _ := cmd.Flags().GetBool("filter")
			filterDays, _ := cmd.Flags().GetInt("filter-days")
			indexCode, _ := cmd.Flags().GetString("index")
			brokerS, _ := cmd.Flags().GetString("broker")
			slippage, _ := cmd.Flags().GetFloat64("slippage-bps")
			holdout, _ := cmd.Flags().GetString("holdout")

			from, err := time.Parse("2006-01-02", fromS)
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			to, err := time.Parse("2006-01-02", toS)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			// The holdout is sealed by refusing to read past it, not by
			// remembering not to. A flag that has to be disabled deliberately
			// is harder to cross by accident than a date in a document.
			if holdout != "" {
				h, err := time.Parse("2006-01-02", holdout)
				if err != nil {
					return fmt.Errorf("--holdout: %w", err)
				}
				if !to.Before(h) {
					return fmt.Errorf(
						"--to %s reaches into the holdout that begins %s; experiment 001 gives the holdout ONE run after provenance exists. Pass --holdout \"\" only when spending it deliberately",
						to.Format(time.DateOnly), h.Format(time.DateOnly))
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
			store := market.NewStore(pool)

			pin := time.Now()
			mk, err := engine.NewStoreMarket(store, source, market.Day(from.Year(), from.Month(), from.Day()),
				market.Day(to.Year(), to.Month(), to.Day()), lookback, n, pin)
			if err != nil {
				return err
			}
			if eq, _ := cmd.Flags().GetBool("equities-only"); eq {
				mk = mk.WithUniverseOptions(market.EquitiesOnly())
			}
			sched, err := cost.NewSchedule(cost.Broker(brokerS))
			if err != nil {
				return err
			}
			broker, err := engine.NewPaper(sched, slippage)
			if err != nil {
				return err
			}
			limits := risk.DefaultLimits()
			limits.SlippageBps = slippage
			// The gate caps how many names the book may hold; a strategy that
			// selects more than the cap would have every extra name rejected and
			// silently become a smaller strategy.
			limits.MaxPositions = top
			// Overriding the floor is how a slippage sensitivity isolates the
			// cost of slippage from its effect on what is tradeable at all.
			// Both are real; conflating them makes neither readable.
			if mn, _ := cmd.Flags().GetFloat64("min-notional"); mn > 0 {
				limits.MinNotional = mn
			}
			gate, err := risk.NewGate(limits, sched)
			if err != nil {
				return err
			}

			var filter strategy.MarketFilter = strategy.AlwaysOn{}
			if useFilter {
				if filter, err = strategy.NewTrendFilter(indexCode, filterDays); err != nil {
					return err
				}
			}
			rule, _ := cmd.Flags().GetString("strategy")
			var strat *strategy.Selector
			switch rule {
			case "momentum":
				strat, err = strategy.NewMomentum(top, formation, skip, filter)
			case "lowvol":
				strat, err = strategy.NewLowVolatility(top, formation, filter)
			case "reversal":
				strat, err = strategy.NewShortTermReversal(top, filter)
			case "equalweight":
				strat, err = strategy.NewEqualWeightUniverse(top, filter)
			default:
				return fmt.Errorf("--strategy %q: want momentum, lowvol, reversal or equalweight", rule)
			}
			if err != nil {
				return err
			}
			if off, _ := cmd.Flags().GetInt("rebalance-offset"); off != 0 {
				if off < 0 {
					return fmt.Errorf("rebalance-offset must not be negative, got %d", off)
				}
				strat.RebalanceOffset = off
			}
			if once, _ := cmd.Flags().GetBool("rebalance-once"); once {
				strat.RebalanceOnce = true
			}
			// The band is the gate's own floor: an order the gate would refuse
			// should not be raised, not merely rejected after the cash is freed.
			strat.MinTradeNotional = gate.Limits().MinNotional
			if strat.MinTradeNotional <= 0 {
				if d, derr := gate.Check(from, risk.Book{}, nil); derr == nil {
					strat.MinTradeNotional = d.MinNotional
				}
			}
			e, err := engine.New(mk, broker, gate, strat)
			if err != nil {
				return err
			}

			// Provenance, computed BEFORE the run so the pin it records is the
			// pin the run actually read at.
			snapshotID, tables, err := store.SnapshotID(cmd.Context(), pin)
			if err != nil {
				return err
			}
			rosters, err := store.RosterDigests(cmd.Context(), pin)
			if err != nil {
				return err
			}
			cfg := map[string]any{
				"strategy": strat.Name(), "source": source, "capital": capital,
				"rule": rule, "equities_only": mustBool(cmd, "equities-only"), "top": top, "formation": formation, "skip": skip,
				"lookback": lookback, "universe_n": n,
				"filter": useFilter, "filter_days": filterDays, "index": indexCode,
				"broker": brokerS, "slippage_bps": slippage,
				"limits": limits, "roster_digests": rosters,
				"from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly),
			}
			cfgDigest, err := entities.DigestOf(cfg)
			if err != nil {
				return err
			}
			configHash := hex.EncodeToString(cfgDigest)

			started := time.Now()
			res, err := e.Run(cmd.Context(), capital)
			if err != nil {
				return err
			}
			benchCode, _ := cmd.Flags().GetString("benchmark")
			if benchCode == "" {
				benchCode = indexCode
			}
			bench, err := mk.IndexCloses(cmd.Context(), benchCode, res.From, res.To)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			rep := report.Build(res, benchCode, bench, capital)
			fmt.Fprintln(out, rep.String())

			if byYear, _ := cmd.Flags().GetBool("by-year"); byYear {
				fmt.Fprintf(out, "\nyear    equity        cash   held    return   benchmark\n")
				for _, y := range rep.ByYear(bench) {
					fmt.Fprintf(out, "%d  %12.0f %11.0f %6d  %+7.2f%%    %+7.2f%%\n",
						y.Year, y.Equity, y.Cash, y.Held, 100*y.Return, 100*y.Benchmark)
				}
			}

			if worst, _ := cmd.Flags().GetInt("worst-days"); worst > 0 {
				// The book's biggest single-session moves beside the
				// benchmark's. A drop the market did not have is not a market
				// move, it is an accounting event.
				type day struct {
					d         time.Time
					book, mkt float64
				}
				bm := map[string]float64{}
				for i := 1; i < len(bench); i++ {
					if bench[i-1].Close > 0 {
						bm[bench[i].Date.Format(time.DateOnly)] = bench[i].Close/bench[i-1].Close - 1
					}
				}
				var days []day
				eq := rep.EquityCurve()
				for i := 1; i < len(eq); i++ {
					if eq[i-1].Equity <= 0 {
						continue
					}
					days = append(days, day{eq[i].Date, eq[i].Equity/eq[i-1].Equity - 1,
						bm[eq[i].Date.Format(time.DateOnly)]})
				}
				sort.Slice(days, func(i, j int) bool { return days[i].book < days[j].book })
				fmt.Fprintf(out, "\nworst single sessions for the book, with the market that day:\n")
				for i := 0; i < worst && i < len(days); i++ {
					fmt.Fprintf(out, "  %s  book %+7.2f%%   market %+6.2f%%   gap %+7.2f%%\n",
						days[i].d.Format(time.DateOnly), 100*days[i].book, 100*days[i].mkt,
						100*(days[i].book-days[i].mkt))
				}
			}

			if len(res.SuspiciousDrops) > 0 {
				fmt.Fprintf(out, "\nUNEXPLAINED DROPS in held positions (%d): fell past %.0f%% in one\n",
					len(res.SuspiciousDrops), 100*33.0/100)
				fmt.Fprintf(out, "session with no corporate action recorded. Almost always an action this\n")
				fmt.Fprintf(out, "project cannot recover, because eod2 is survivor-only and a delisted\n")
				fmt.Fprintf(out, "company has no adjusted series to derive a factor from.\n")
				// Priced, not just counted. The residual is what the book still
				// held after the fall; if the true event destroyed the position
				// outright -- which is the worst case for an action this project
				// cannot recover -- that residual is what the result overstates by.
				eqAt := make(map[string]float64, len(res.Equity))
				for _, e := range res.Equity {
					eqAt[e.Date.Format(time.DateOnly)] = e.Equity
				}
				shown := res.SuspiciousDrops
				sort.Slice(shown, func(i, j int) bool { return shown[i].Change < shown[j].Change })
				worst := 0.0
				for i, d := range shown {
					residual := float64(d.Quantity) * d.To
					share := 0.0
					if eq := eqAt[d.Date.Format(time.DateOnly)]; eq > 0 {
						share = residual / eq
					}
					worst += share
					if i >= 15 {
						fmt.Fprintf(out, "  ... and %d more\n", len(shown)-15)
						continue
					}
					fmt.Fprintf(out, "  %s  %-14s %10.2f -> %8.2f  %+7.1f%%   residual %12.0f  %5.2f%% of book\n",
						d.Date.Format(time.DateOnly), d.Scrip, d.From, d.To, 100*d.Change,
						residual, 100*share)
				}
				fmt.Fprintf(out, "  worst case if every one of them was in truth a total loss: %.2f%% of the book,\n", 100*worst)
				fmt.Fprintf(out, "  summed at the equity of each date (an upper bound; they did not all happen at once).\n")
			}

			if len(res.Adjustments) > 0 {
				// What the corporate action layer actually did to the book. A
				// correct action is equity-neutral -- the share count rises by
				// the ratio and the price falls by it -- so this is the place a
				// wrong ratio or a misdated ex-date shows up as a position that
				// changed size for no reason.
				byScrip := map[string]int{}
				for _, a := range res.Adjustments {
					byScrip[a.Scrip]++
				}
				fmt.Fprintf(out, "\nCORPORATE ACTIONS applied to the book (%d across %d names):\n",
					len(res.Adjustments), len(byScrip))
				shown := append([]engine.AdjustmentApplied(nil), res.Adjustments...)
				sort.Slice(shown, func(i, j int) bool { return shown[i].Ratio > shown[j].Ratio })
				for i, a := range shown {
					if i >= 12 {
						fmt.Fprintf(out, "  ... and %d more\n", len(shown)-12)
						break
					}
					fmt.Fprintf(out, "  %-14s ratio %8.4f  %7d -> %7d shares  cash %8.2f\n",
						a.Scrip, a.Ratio, a.FromQty, a.ToQty, a.CashPaid)
				}
			}

			j := strat.Journal()
			fmt.Fprintf(out, "\nstrategy journal:\n")
			fmt.Fprintf(out, "  rebalances              %14d\n", j.Rebalances)
			fmt.Fprintf(out, "  risk-off sessions       %14d  (liquidations %d)\n", j.RiskOffSessions, j.Liquidations)
			fmt.Fprintf(out, "  unreadable sessions     %14d  (held, traded nothing)\n", j.UnknownSessions)
			fmt.Fprintf(out, "  returns refused (fence) %14d\n", j.ReturnsRefused)
			fmt.Fprintf(out, "  returns absent          %14d\n", j.ReturnsAbsent)
			if held := res.Final.Held(); len(held) > 0 {
				fmt.Fprintf(out, "\nfinal holdings (%d):\n", len(held))
				for _, h := range held {
					fmt.Fprintf(out, "  %-14s qty %8d  basis %12.2f  last mark %10.2f\n",
						h.Scrip, h.Quantity, h.CostBasis, h.LastMark)
				}
			}
			runID, err := market.NewRunID()
			if err != nil {
				return err
			}
			finished := time.Now()
			record, _ := cmd.Flags().GetBool("record")
			fmt.Fprintf(out, "\nprovenance\n")
			fmt.Fprintf(out, "  run_id       %s\n", runID)
			fmt.Fprintf(out, "  git_sha      %s\n", gitSHA())
			fmt.Fprintf(out, "  config_hash  %s\n", configHash)
			fmt.Fprintf(out, "  snapshot_id  %s\n", snapshotID)
			for _, t := range tables {
				fmt.Fprintf(out, "    %-14s %10d rows  %s\n", t.Table, t.Rows, t.Digest[:16])
			}
			fmt.Fprintf(out, "  ingest pin   %s\n", pin.Format(time.RFC3339))
			fmt.Fprintf(out, "  ran in       %s\n", finished.Sub(started).Round(time.Second))

			// A repeat of an identical run is not new evidence, and the only
			// way to know is to have recorded the first one.
			if prior, err := store.PriorRuns(cmd.Context(), configHash, snapshotID); err == nil && len(prior) > 0 {
				fmt.Fprintf(out, "\n  NOTE: %d earlier run(s) share this exact config and snapshot:\n", len(prior))
				for _, r := range prior {
					fmt.Fprintf(out, "    %s  %s  %s..%s\n", r.StartedAt.Format(time.RFC3339),
						r.RunID, r.PeriodFrom.Format(time.DateOnly), r.PeriodTo.Format(time.DateOnly))
				}
				fmt.Fprintf(out, "  This run is a repetition, not independent evidence.\n")
			}

			if !record {
				fmt.Fprintf(out, "\n  not recorded (--record=false); this run leaves no audit trail\n")
				return nil
			}
			if err := store.RecordRun(cmd.Context(), market.Run{
				RunID: runID, Mode: "backtest", Strategy: strat.Name(),
				GitSHA: gitSHA(), ConfigHash: configHash, SnapshotID: snapshotID,
				IngestPin: pin, PeriodFrom: res.From, PeriodTo: res.To,
				Result: map[string]any{
					"net_cagr": rep.NetCAGR, "benchmark": benchCode,
					"benchmark_cagr": rep.BenchmarkCAGR, "excess_cagr": rep.ExcessCAGR,
					"max_drawdown": rep.MaxDrawdown, "final_equity": rep.FinalNet,
					"total_costs": rep.TotalCosts, "trades": len(rep.Trades),
					"trade_mean": rep.TradeMean, "trade_stddev": rep.TradeStdDev,
					"turnover": rep.Turnover, "slippage_bps": slippage,
				},
				StartedAt: started, FinishedAt: finished,
			}); err != nil {
				return err
			}
			fmt.Fprintf(out, "  recorded as run %s\n", runID)
			return nil
		},
	}
	cmd.Flags().String("from", "2013-01-01", "first session")
	cmd.Flags().String("to", "2021-12-31", "last session")
	cmd.Flags().String("holdout", "2022-01-01", "refuse to read at or past this date; \"\" to spend the holdout")
	cmd.Flags().Float64("capital", 500000, "starting capital in rupees")
	cmd.Flags().Int("worst-days", 0, "print the N worst single sessions beside the market")
	cmd.Flags().Int("rebalance-offset", 0,
		"shift every rebalance N sessions past the month end; sweep 0..20 and average to remove the arbitrary calendar (see experiment 005)")
	cmd.Flags().Bool("rebalance-once", false,
		"buy the first selection and never trade again; isolates rebalancing from signal")
	cmd.Flags().Bool("by-year", false, "print the book and the benchmark at each year end")
	cmd.Flags().Bool("equities-only", false,
		"drop fund units (INF/IN9 ISINs) from the universe; NSE lists ETFs in the same segment as shares")
	cmd.Flags().String("strategy", "momentum", "momentum, lowvol, reversal or equalweight (see docs/experiments)")
	cmd.Flags().Int("top", 20, "positions held")
	cmd.Flags().Int("formation", 12, "months back for the far end of the ranking window")
	cmd.Flags().Int("skip", 1, "months skipped at the near end")
	cmd.Flags().Int("lookback", 125, "universe lookback in sessions")
	cmd.Flags().Int("n", 500, "universe size")
	cmd.Flags().String("source", market.SourceBhavcopy, "bar source")
	cmd.Flags().Bool("filter", true, "apply the 200-day market trend filter")
	cmd.Flags().Int("filter-days", 200, "trend filter window in sessions")
	cmd.Flags().String("index", nseindex.Nifty50, "index the trend filter reads")
	cmd.Flags().String("benchmark", "", "index to compare against (default: the filter index). The universe is the top 500 by turnover, which sits well below the Nifty 50 in market cap, so NIFTY500 is the fairer comparison and NIFTY50 flatters the strategy by the size premium.")
	cmd.Flags().String("broker", string(cost.Zerodha), "broker charge schedule")
	cmd.Flags().Float64("slippage-bps", 0, "slippage per leg in basis points")
	cmd.Flags().Float64("min-notional", 0, "override the derived position floor (0 = derive it)")
	cmd.Flags().Bool("record", true, "write a runs row so the result can be replayed and repeats detected")
	cmd.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	return cmd
}

func newAdjustmentsCmd() *cobra.Command {
	adj := &cobra.Command{
		Use:   "adjustments",
		Short: "Corporate actions: detect share-count changes and report coverage",
	}
	detect := &cobra.Command{
		Use:   "detect",
		Short: "Derive corporate actions from the adjusted and unadjusted series and store them",
		RunE: func(cmd *cobra.Command, args []string) error {
			minStep, _ := cmd.Flags().GetFloat64("min-step")
			apply, _ := cmd.Flags().GetBool("write")
			url, err := databaseURL(cmd)
			if err != nil {
				return err
			}
			pool, err := db.Connect(cmd.Context(), url)
			if err != nil {
				return err
			}
			defer pool.Close()
			store := market.NewStore(pool)
			pin := time.Now()
			out := cmd.OutOrStdout()

			both, bhavOnly, err := store.AdjustmentCoverage(cmd.Context(), pin)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "coverage: %d entities have both series and can have factors derived;\n", both)
			fmt.Fprintf(out, "          %d have only the unadjusted one and cannot -- eod2 is survivor-only,\n", bhavOnly)
			fmt.Fprintf(out, "          so a delisted company's corporate actions stay invisible.\n\n")

			fracTol, _ := cmd.Flags().GetFloat64("fraction-tolerance")
			adjs, sum, err := store.DetectAdjustments(cmd.Context(), minStep, fracTol, pin)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%d ratio steps above %.1f%%\n", sum.Steps, 100*minStep)
			fmt.Fprintf(out, "  %d match a share-count fraction AND the price confirms it -- kept\n", sum.ShareActions)
			fmt.Fprintf(out, "  %d match none and are discarded: eod2 adjusts for dividends too,\n", sum.Unclassified)
			fmt.Fprintf(out, "     and a dividend pays cash rather than changing what you hold\n")
			fmt.Fprintf(out, "  %d match a fraction the PRICE DOES NOT CONFIRM and are discarded:\n", sum.Uncorroborated)
			fmt.Fprintf(out, "     the two sources disagree about which session the action landed, and\n")
			fmt.Fprintf(out, "     multiplying a share count with no matching price fall invents money\n\n")

			buckets := map[string]int{}
			for _, a := range adjs {
				buckets[bucketRatio(a.Ratio)]++
			}
			var keys []string
			for k := range buckets {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return buckets[keys[i]] > buckets[keys[j]] })
			for i, k := range keys {
				if i >= 12 {
					break
				}
				fmt.Fprintf(out, "  ratio %-8s %5d\n", k, buckets[k])
			}
			if !apply {
				fmt.Fprintf(out, "\nnot written (--write=false)\n")
				return nil
			}
			n, err := store.InsertAdjustments(cmd.Context(), market.SourceBhavcopy, adjs)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\nwrote %d new or corrected adjustment rows\n", n)
			return nil
		},
	}
	detect.Flags().Float64("min-step", 0.02,
		"smallest change in the two sources' ratio that counts as a corporate action; below this the two round differently and the ratio wobbles")
	detect.Flags().Float64("fraction-tolerance", 0.01,
		"how close a ratio must be to a simple fraction to count as a share action")
	detect.Flags().Bool("write", false, "write the detected adjustments to the store")
	detect.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	adj.AddCommand(detect)
	return adj
}

// bucketRatio labels a measured ratio with the corporate action it looks like,
// for a histogram that a human can sanity-check at a glance. A 1:1 bonus
// doubles the share count; a 1:2 consolidation halves it.
func bucketRatio(r float64) string {
	for _, known := range []struct {
		v     float64
		label string
	}{
		{2, "2 (1:1)"}, {3, "3 (2:1)"}, {5, "5 (1:5 split)"}, {10, "10 (1:10)"},
		{1.5, "1.5 (1:2)"}, {4, "4"}, {6, "6"}, {0.5, "0.5 (consolidate)"},
		{1.2, "1.2 (1:5)"}, {1.1, "1.1 (1:10)"}, {20, "20"},
	} {
		if math.Abs(r/known.v-1) < 0.02 {
			return known.label
		}
	}
	return fmt.Sprintf("other %.3f", r)
}
