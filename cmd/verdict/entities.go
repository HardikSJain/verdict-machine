package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/HardikSJain/verdict-machine/internal/db"
	"github.com/HardikSJain/verdict-machine/internal/market/entities"
)

// The four verbs that mint the canonical-entity map, and the one that unmints
// it.
//
// The order is the whole safety argument: propose is read-only and produces a
// file; a human reads the diff and merges it; apply writes rows and stamps
// the reviewed file's sha256 into every one; retract writes one more row when
// the answer turns out to be wrong. Nothing here runs after an ingest, and
// nothing writes a link the file does not contain.

func newEntitiesProposeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "propose",
		Short: "Read-only: generate succession candidates, run the gates, write the roster and the review file",
		Long: "Generate candidate successions and run gates G0-G6 over them.\n\n" +
			"Read-only. It writes two files and nothing to the store: the roster of lines\n" +
			"that passed every gate, and a review file with one record per candidate --\n" +
			"accepted or quarantined -- carrying every gate result and the close, turnover\n" +
			"and delivery ratios across the boundary.\n\n" +
			"It refuses to emit anything at all while the archive span holds an unsettled\n" +
			"date (G4a). G4 counts sessions out of bars, so a real NSE session the store\n" +
			"has not fetched reads as \"zero sessions between\" -- and the 2017 Tube\n" +
			"Investments demerger, which is disjoint, same-issuer and serial-increasing,\n" +
			"is then stopped only by the 31 live sessions in its gap. Do not run this\n" +
			"mid-backfill.",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, _ := cmd.Flags().GetString("out")
			reviewPath, _ := cmd.Flags().GetString("review-out")
			ratifiedBy, _ := cmd.Flags().GetString("ratified-by")
			asOfIngestS, _ := cmd.Flags().GetString("as-of-ingest")
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

			started := time.Now()
			roster, candidates, counts, err := entities.Propose(cmd.Context(), pool, entities.ProposeOptions{
				AsOfIngest: asOfIngest,
				RatifiedBy: ratifiedBy,
				RatifiedAt: ratifiedAt(ratifiedBy),
			})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "# archive %s..%s, %d sessions, %d symbols with bhavcopy bars, map pinned at %s\n",
				counts.ArchiveFrom.Format("2006-01-02"), counts.ArchiveTo.Format("2006-01-02"),
				counts.Sessions, counts.Symbols, asOfIngest.Format(time.RFC3339))
			fmt.Fprintf(w, "# ingest_log: %d unsettled dates in the span, %d pending (too recent for the last run to settle)\n",
				counts.ArchiveUnsettled, counts.ArchivePending)
			if counts.LinkOverlayPresent {
				fmt.Fprintf(w, "# symbol_links: %d pair(s) currently retracted, quarantined here rather than silently re-proposed\n",
					counts.RetractedPairs)
			} else {
				fmt.Fprintln(w, "# symbol_links does not exist in this store, so there was no retraction memory to consult:"+
					" a pair a human already withdrew cannot be recognised here. Run `verdict migrate` first.")
			}
			fmt.Fprintf(w, "candidates\t%d\naccepted\t%d\nquarantined\t%d\n",
				counts.Candidates, counts.Accepted, counts.Quarantined)
			for _, g := range entities.ReportOrder {
				fmt.Fprintf(w, "  %-24s rejected %4d  (first failure for %d)\n", g, counts.AnyFailure[g], counts.FirstFailure[g])
			}

			if err := writeFile(out, func(path string) error {
				b, err := roster.Marshal()
				if err != nil {
					return err
				}
				return os.WriteFile(path, b, 0o644)
			}); err != nil {
				return err
			}
			if err := writeFile(reviewPath, func(path string) error {
				f, err := os.Create(path)
				if err != nil {
					return err
				}
				defer f.Close()
				return entities.WriteReview(f, candidates)
			}); err != nil {
				return err
			}
			digest, err := roster.ComputeDigest()
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "# wrote %s (%d links, sha256 %s) and %s (%d records) in %s\n",
				out, len(roster.Links), hex.EncodeToString(digest), reviewPath, len(candidates),
				time.Since(started).Round(time.Millisecond))
			fmt.Fprintf(w, "# nothing was written to the store. Review the diff, then run `verdict entities apply --roster %s`.\n", out)
			return nil
		},
	}
	cmd.Flags().String("out", "roster.json", "where to write the roster")
	cmd.Flags().String("review-out", filepath.Join("data", "succession-review.jsonl"), "where to write the per-candidate review file")
	cmd.Flags().String("ratified-by", "", "handle to record as the ratifier on every generated line (default: unratified)")
	cmd.Flags().String("as-of-ingest", "", "pin every read at this ingest timestamp: RFC3339, or YYYY-MM-DD for midnight UTC (default now)")
	return cmd
}

// ratifiedAt dates a ratification only when there is a ratifier to date. A
// generated file is not ratified by having been generated.
func ratifiedAt(ratifiedBy string) string {
	if ratifiedBy == "" {
		return ""
	}
	return time.Now().UTC().Format("2006-01-02")
}

func writeFile(path string, write func(string) error) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return write(path)
}

func newEntitiesLinkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Append one human-decided line to the roster file (writes nothing to the store)",
		Long: "Append a hand-decided succession to the roster.\n\n" +
			"This is how a QUARANTINED pair gets into the map: an INF fund-unit transfer,\n" +
			"a long-gap relisting -- the cases no gate admits. The pair is measured against\n" +
			"the store so the line carries the same evidence a generated one does, and the\n" +
			"gate failures are recorded rather than hidden.\n\n" +
			"It writes to the file only. Design 5.6: no quarantined pair may be promoted\n" +
			"without an external NSE corporate-action source, which is why --ratified-by and\n" +
			"--note are required and why the note should name the circular.",
		RunE: func(cmd *cobra.Command, args []string) error {
			pred, _ := cmd.Flags().GetString("pred")
			succ, _ := cmd.Flags().GetString("succ")
			note, _ := cmd.Flags().GetString("note")
			ratifiedBy, _ := cmd.Flags().GetString("ratified-by")
			rosterPath, _ := cmd.Flags().GetString("roster")
			if pred == "" || succ == "" {
				return fmt.Errorf("--pred and --succ are required")
			}
			if note == "" || ratifiedBy == "" {
				return fmt.Errorf("--note and --ratified-by are required: a hand-written line is the highest-risk row the store can hold and no gate stands behind it")
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

			c, err := entities.EvaluatePair(cmd.Context(), pool, pred, succ, time.Now())
			if err != nil {
				return err
			}
			r, err := entities.AppendManual(rosterPath, c, ratifiedBy, note)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s -> %s, boundary %s, ticker %s\n", pred, succ, c.SuccessorSpn[0], c.TickerAfter)
			fmt.Fprintf(w, "  spans %s..%s and %s..%s, %d sessions in the gap, %d calendar days, boundary close ratio %.4f\n",
				c.PredecessorSpn[0], c.PredecessorSpn[1], c.SuccessorSpn[0], c.SuccessorSpn[1],
				c.Gates.SessionsBetween, c.Gates.CalendarDaysBetween, c.Gates.BoundaryCloseRatio)
			if len(c.FailedGates) > 0 {
				fmt.Fprintf(w, "  gates this pair FAILS: %v -- recorded on the line, not hidden by it\n", c.FailedGates)
			}
			fmt.Fprintf(w, "# %s now holds %d links. Nothing was written to the store.\n", rosterPath, len(r.Links))
			return nil
		},
	}
	cmd.Flags().String("pred", "", "predecessor ISIN")
	cmd.Flags().String("succ", "", "successor ISIN")
	cmd.Flags().String("note", "", "what settled this: name the NSE circular")
	cmd.Flags().String("ratified-by", "", "who decided")
	cmd.Flags().String("roster", "roster.json", "roster file to append to")
	return cmd
}

func newEntitiesApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Validate a roster and INSERT its links, stamping the roster's sha256 into every row",
		Long: "Write the reviewed roster to symbol_links.\n\n" +
			"The gates run again here, against the file on disk, so rows are never written\n" +
			"on the strength of a measurement taken at generation time. Every row carries\n" +
			"the roster's sha256 in roster_sha, which is what points an irreversible merge\n" +
			"back at a diff a human approved.\n\n" +
			"This is the irreversible half. symbol_links has no DELETE: a wrong line is\n" +
			"corrected by `entities retract`, which writes one more row, and every answer\n" +
			"given while the wrong link stood stays permanently replayable at its own\n" +
			"timestamp. Use --dry-run first.",
		RunE: func(cmd *cobra.Command, args []string) error {
			rosterPath, _ := cmd.Flags().GetString("roster")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			seededBy, _ := cmd.Flags().GetString("seeded-by")
			roster, digest, err := entities.Load(rosterPath)
			if err != nil {
				return err
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

			res, err := entities.Apply(cmd.Context(), pool, roster, digest, seededBy, dryRun)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			unratified := 0
			for _, l := range roster.Links {
				if l.RatifiedBy == "" {
					unratified++
				}
			}
			relinked := 0
			for _, row := range res.Rows {
				fmt.Fprintf(w, "%s\tsymbol %d -> entity %d (%s)\tboundary %s\t%s\n",
					row.ISIN, row.SymbolID, row.EntityID, row.EntityISIN, row.Boundary.Format("2006-01-02"), row.Reason)
				if row.WasRetracted {
					relinked++
					fmt.Fprintf(w, "  WARNING: symbol %d (%s) carries a retraction row -- this link was WITHDRAWN by a human and is being written back.\n",
						row.SymbolID, row.ISIN)
				}
			}
			verb := "inserted"
			if dryRun {
				verb = "would insert"
			}
			fmt.Fprintf(w, "# %s %d rows, skipped %d already-linked, roster %s sha256 %s\n",
				verb, len(res.Rows), len(res.Skipped), rosterPath, hex.EncodeToString(digest))
			if unratified > 0 {
				fmt.Fprintf(w, "# WARNING: %d of %d roster lines name no ratifier. The rows are written; the file does not say who approved them.\n",
					unratified, len(roster.Links))
			}
			if relinked > 0 {
				fmt.Fprintf(w, "# WARNING: %d row(s) re-link a pair that was retracted. `propose` regenerates the roster out of bars alone,"+
					" so a withdrawn pair reappears in it every month -- check that this was meant.\n", relinked)
			}
			return nil
		},
	}
	cmd.Flags().String("roster", "roster.json", "roster file to apply")
	cmd.Flags().Bool("dry-run", false, "validate, plan and roll back without committing")
	cmd.Flags().String("seeded-by", "", "value for seeded_by (default 'entities apply v1')")
	return cmd
}

func newEntitiesRetractCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retract",
		Short: "Dissolve an entity by writing one retraction row per member",
		Long: "Undo a merge.\n\n" +
			"It resolves --entity through the map and writes a retraction row for EVERY\n" +
			"member whose entity_id differs from its own symbol_id, in one transaction.\n" +
			"From that moment those symbols resolve to themselves again.\n\n" +
			"It is entity-scoped and that is a correction, not a preference: a symbol-scoped\n" +
			"retract naming the ROOT inserts a row, reports success and leaves the merge\n" +
			"standing, because each member's own latest row still points at the root. That\n" +
			"is a wrong answer with no error in the one code path whose entire job is to\n" +
			"correct a wrong answer. --symbol survives for the single-member case and\n" +
			"refuses, naming the other members, when the named symbol is a root.\n\n" +
			"The bad row is not deleted -- it cannot be, and it should not be. A query\n" +
			"pinned before the link replays the fragmented answer, one pinned between the\n" +
			"link and the retraction replays the merged answer, and one pinned after gets\n" +
			"the corrected answer. All three stay reproducible forever.",
		RunE: func(cmd *cobra.Command, args []string) error {
			entity, _ := cmd.Flags().GetString("entity")
			symbol, _ := cmd.Flags().GetString("symbol")
			note, _ := cmd.Flags().GetString("note")
			if (entity == "") == (symbol == "") {
				return fmt.Errorf("pass exactly one of --entity or --symbol (an id or an ISIN)")
			}
			ref, symbolScoped := entity, false
			if symbol != "" {
				ref, symbolScoped = symbol, true
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

			res, err := entities.Retract(cmd.Context(), pool, ref, note, "", symbolScoped)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			sort.Slice(res.Members, func(i, j int) bool { return res.Members[i].SymbolID < res.Members[j].SymbolID })
			for _, m := range res.Members {
				fmt.Fprintf(w, "retracted\tsymbol %d (%s)\twas in entity %d\n", m.SymbolID, m.ISIN, m.WasIn)
			}
			// Say what happened, not what the verb is usually for. A
			// symbol-scoped run detaches one member; calling that "entity N
			// dissolved" would be the same class of untrue confirmation
			// design 4.6 exists to remove.
			if res.SymbolScoped {
				fmt.Fprintf(w, "# symbol %d detached from entity %d: %d linked member(s) remain\n",
					res.Members[0].SymbolID, res.EntityID, res.Remaining)
			} else {
				fmt.Fprintf(w, "# entity %d dissolved: %d members resolve to themselves from now on\n", res.EntityID, len(res.Members))
			}
			fmt.Fprintln(w, "# remove these lines from the roster and commit, or the next `apply` writes them back:")
			for _, m := range res.Members {
				fmt.Fprintf(w, "#   successor %s\n", m.ISIN)
			}
			return nil
		},
	}
	cmd.Flags().String("entity", "", "entity to dissolve: a symbol_id or an ISIN")
	cmd.Flags().String("symbol", "", "single linked member to detach: a symbol_id or an ISIN")
	cmd.Flags().String("note", "", "why")
	return cmd
}
