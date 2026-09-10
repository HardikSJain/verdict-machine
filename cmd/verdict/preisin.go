package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/HardikSJain/verdict-machine/internal/db"
	"github.com/HardikSJain/verdict-machine/internal/market"
	"github.com/HardikSJain/verdict-machine/internal/market/bhavcopy"
	"github.com/HardikSJain/verdict-machine/internal/market/preisin"
)

// newPreISINCmd proposes an identity for every ticker in NSE's archive from
// before it printed ISINs.
//
// It PROPOSES rather than applies, following the same two-step the succession
// roster uses, and for the same reason: an identity decision is the one kind
// of error this project cannot detect after the fact. A wrong price is visible
// in a chart. Two companies welded into one series looks exactly like a
// company with an eventful history, and every backtest that touches it is
// quietly wrong forever.
func newPreISINCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preisin",
		Short: "Identity for NSE's pre-July-2011 archive, which printed no ISIN",
	}
	propose := &cobra.Command{
		Use:   "propose",
		Short: "Scan the pre-ISIN archive and propose an identity for every ticker",
		RunE: func(cmd *cobra.Command, args []string) error {
			fromS, _ := cmd.Flags().GetString("from")
			toS, _ := cmd.Flags().GetString("to")
			out, _ := cmd.Flags().GetString("out")
			delay, _ := cmd.Flags().GetDuration("delay")
			maxGap, _ := cmd.Flags().GetInt("max-gap")

			from, err := time.Parse(time.DateOnly, fromS)
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			to, err := time.Parse(time.DateOnly, toS)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			if from.Before(bhavcopy.ArchiveStart) {
				fmt.Fprintf(cmd.OutOrStdout(), "note: the archive starts %s; clamping\n",
					bhavcopy.ArchiveStart.Format(time.DateOnly))
				from = bhavcopy.ArchiveStart
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
			w := cmd.OutOrStdout()

			// The ISIN era, as NSE published it. This is the only source of
			// truth for what a ticker meant; a security master would answer
			// for survivors only.
			bridge, err := store.PreISINBridge(cmd.Context(), market.SourceBhavcopy, pin)
			if err != nil {
				return err
			}
			isinSessions, err := store.Sessions(cmd.Context(), market.SourceBhavcopy,
				market.Day(1900, 1, 1), market.Day(2100, 1, 1), pin)
			if err != nil {
				return err
			}
			if len(isinSessions) == 0 {
				return fmt.Errorf("the store holds no sessions; ingest the ISIN era before proposing identity for the one before it")
			}
			fmt.Fprintf(w, "ISIN era: %d tickers over %d sessions from %s\n\n",
				len(bridge), len(isinSessions), isinSessions[0].Format(time.DateOnly))

			// Walk the pre-ISIN archive for ticker sightings only. Bars are
			// not stored here: identity has to be decided before anything can
			// be, which is the whole reason this command exists.
			f := bhavcopy.NewFetcher()
			lastSeen := map[string]time.Time{}
			var preSessions []time.Time
			fetched, absent := 0, 0
			for d := market.Day(from.Year(), from.Month(), from.Day()); !d.After(to); d = d.AddDate(0, 0, 1) {
				bars, found, err := f.Fetch(cmd.Context(), d)
				if err != nil {
					return fmt.Errorf("%s: %w", d.Format(time.DateOnly), err)
				}
				if !found {
					absent++
					time.Sleep(delay)
					continue
				}
				fetched++
				preSessions = append(preSessions, d)
				for _, b := range bars {
					if prev, ok := lastSeen[b.Ticker]; !ok || b.Date.After(prev) {
						lastSeen[b.Ticker] = b.Date
					}
				}
				if fetched%100 == 0 {
					fmt.Fprintf(w, "  %s  %d sessions, %d tickers so far\n",
						d.Format(time.DateOnly), fetched, len(lastSeen))
				}
				time.Sleep(delay)
			}
			fmt.Fprintf(w, "pre-ISIN archive: %d sessions, %d absent, %d distinct tickers\n\n",
				fetched, absent, len(lastSeen))
			if fetched == 0 {
				return fmt.Errorf("no pre-ISIN sessions fetched in %s..%s", fromS, toS)
			}

			// One calendar spanning both eras, so a gap across the boundary is
			// counted in sessions the exchange actually held.
			calendar := append(append([]time.Time{}, preSessions...), isinSessions...)
			sort.Slice(calendar, func(i, j int) bool { return calendar[i].Before(calendar[j]) })

			obs := make([]preisin.Observation, 0, len(lastSeen))
			for ticker, last := range lastSeen {
				o := preisin.Observation{Ticker: ticker, LastPreISIN: last}
				if b, ok := bridge[ticker]; ok {
					o.FirstISINEra, o.ISINEraISIN = b.Date, b.ISIN
				}
				obs = append(obs, o)
			}
			assignments, sum := preisin.Resolve(obs, preisin.SessionGapFunc(calendar), maxGap)

			fmt.Fprintf(w, "  bridged      %5d  (traded across the boundary; take NSE's own ISIN)\n", sum.Bridged)
			fmt.Fprintf(w, "  dead         %5d  (never traded once ISINs existed; synthetic identity)\n", sum.Dead)
			fmt.Fprintf(w, "  quarantined  %5d  (gap too wide to tell suspension from reassignment)\n", sum.Quarantined)
			fmt.Fprintf(w, "  total        %5d\n\n", sum.Total())

			if sum.Quarantined > 0 {
				fmt.Fprintf(w, "quarantined, widest gaps first -- these are NOT merged and need a human.\n")
				fmt.Fprintf(w, "Most are probably SERIES MIGRATIONS rather than reassigned tickers: this\n")
				fmt.Fprintf(w, "archive keeps only EQ rows, and NSE moves stocks to the BE surveillance\n")
				fmt.Fprintf(w, "segment and back, so a company trading every day can vanish from it for\n")
				fmt.Fprintf(w, "months. Ingesting BE would resolve most of these; see package preisin.\n")
				q := make([]preisin.Assignment, 0, sum.Quarantined)
				for _, a := range assignments {
					if a.Status == preisin.Quarantined {
						q = append(q, a)
					}
				}
				sort.Slice(q, func(i, j int) bool { return q[i].GapSessions > q[j].GapSessions })
				for i, a := range q {
					if i >= 20 {
						fmt.Fprintf(w, "  ... and %d more in %s\n", len(q)-20, out)
						break
					}
					fmt.Fprintf(w, "  %-14s last %s  reappears %s  gap %5d sessions\n",
						a.Ticker, a.LastPreISIN.Format(time.DateOnly),
						a.FirstISIN.Format(time.DateOnly), a.GapSessions)
				}
				fmt.Fprintln(w)
			}

			blob, err := json.MarshalIndent(map[string]any{
				"from": fromS, "to": toS, "max_gap_sessions": maxGap,
				"generated_at": time.Now().UTC().Format(time.RFC3339),
				"summary": map[string]int{"bridged": sum.Bridged, "dead": sum.Dead,
					"quarantined": sum.Quarantined, "total": sum.Total()},
				"assignments": assignments,
			}, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(out, append(blob, '\n'), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(w, "wrote %s\n", out)
			fmt.Fprintf(w, "\nNOT APPLIED. Nothing is stored until these decisions are reviewed:\n")
			fmt.Fprintf(w, "a wrong identity welds two companies into one price series, which looks\n")
			fmt.Fprintf(w, "exactly like an eventful history and is invisible from then on.\n")
			return nil
		},
	}
	propose.Flags().String("from", "2007-01-01", "first session to scan")
	propose.Flags().String("to", "2011-07-03", "last session before NSE began printing ISINs")
	propose.Flags().String("out", "data/preisin-identity.json", "where to write the proposal")
	propose.Flags().Duration("delay", 400*time.Millisecond, "pause between archive requests")
	propose.Flags().Int("max-gap", preisin.MaxBridgeGapSessions,
		"widest boundary gap in SESSIONS that still counts as direct evidence")
	propose.Flags().String("database-url", "", "Postgres URL (default $VERDICT_DATABASE_URL)")
	cmd.AddCommand(propose)
	return cmd
}
