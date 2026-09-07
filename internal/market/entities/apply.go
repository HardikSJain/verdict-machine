package entities

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Writing the map, and undoing it.
//
// linkLockKey is a NEW advisory-lock key beside symbolRegistryLockKey
// (84031178) and the test lock (84031177). apply and retract deliberately do
// NOT take the symbols registry lock: the write is under a second against a
// table nothing else writes, so it is safe to run while a backfill is in
// flight and it must not block EnsureSymbols.
//
// Concurrency safety for the WRITER is not completeness safety for the
// READER, and the two must not be confused: `propose` carries no such licence
// (see G4a), because it counts sessions out of bars and an archive assembled
// by a resumable HTTP backfill is not complete just because nothing is
// writing to it right now.
const linkLockKey = 84031179

// SeededBy is the seeded_by value apply stamps on the rows it writes.
const SeededBy = "entities apply v1"

// LinkRow is one row destined for symbol_links.
//
// WasRetracted is set when the symbol this row links already carries a
// retraction row: the link is being written BACK after a human withdrew it.
// Design 8.4 requires that re-link to remain possible, so it is not refused
// -- but `propose` regenerates the roster out of bars alone and a retracted
// pair reappears in it every month, so doing it by accident must be
// impossible. The CLI prints a warning for every such row.
type LinkRow struct {
	SymbolID       int64
	EntityID       int64
	PredecessorID  int64
	Boundary       time.Time
	Reason         string
	Note           string
	Evidence       []byte
	ISIN           string
	PredecessorSIN string
	EntityISIN     string
	WasRetracted   bool
}

// ApplyResult reports what apply wrote, or would have written.
type ApplyResult struct {
	Rows    []LinkRow
	Skipped []LinkRow // already resolved to the intended entity
	DryRun  bool
}

// Apply validates the roster and INSERTs its links, stamping the roster's
// sha256 into every row.
//
// The rows are written in ONE transaction under linkLockKey. A row whose
// symbol already resolves to the intended entity is skipped rather than
// re-written: a second apply of the same roster would otherwise double the
// table for no change in meaning, and symbol_links has no DELETE.
//
// A symbol that currently resolves to a DIFFERENT entity is an error, not an
// overwrite. Re-pointing an existing member is a decision with a name --
// retract, then apply -- and doing it silently inside apply would let a
// roster edit move a company between entities with nothing recording that a
// previous answer was withdrawn.
func Apply(ctx context.Context, pool *pgxpool.Pool, r *Roster, digest []byte, seededBy string, dryRun bool) (*ApplyResult, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if len(digest) == 0 {
		return nil, fmt.Errorf("apply: refusing to write rows with no roster digest; roster_sha is the provenance of an irreversible merge")
	}
	if seededBy == "" {
		seededBy = SeededBy
	}
	rows, err := plan(ctx, pool, r)
	if err != nil {
		return nil, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", linkLockKey); err != nil {
		return nil, err
	}
	current, err := currentMap(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := checkEntityNames(rows, current); err != nil {
		return nil, err
	}
	retracted, err := retractedSymbols(ctx, tx, rows)
	if err != nil {
		return nil, err
	}
	out := &ApplyResult{DryRun: dryRun}
	for _, row := range rows {
		row.WasRetracted = retracted[row.SymbolID]
		if got, ok := current[row.SymbolID]; ok && got.EntityID != row.SymbolID {
			if got.EntityID != row.EntityID {
				return nil, fmt.Errorf(
					"apply: %s (symbol %d) already resolves to entity %d, and this roster puts it in entity %d; "+
						"retract the existing link first -- re-pointing a member silently would move a company between entities with nothing recording that the old answer was withdrawn",
					row.ISIN, row.SymbolID, got.EntityID, row.EntityID)
			}
			// Same entity, but "already linked" is not the same question as
			// "linked the same way". boundary is not provenance -- design 0
			// withdrew that claim -- it is the only column that says which
			// member of an entity is in force on a date, so a roster whose
			// boundary has been CORRECTED must not be skipped as a no-op. It
			// is also reachable from the design's own monthly ops flow: bars
			// is insert-only but a backfill can still add EARLIER sessions,
			// which moves a successor's first bar and hence effective_from.
			if diff := got.differsFrom(row); diff != "" {
				return nil, fmt.Errorf(
					"apply: %s (symbol %d) is already linked into entity %d, but not the way this roster says: %s. "+
						"That is a correction to a load-bearing answer, not a no-op -- boundary decides which member labels a session. "+
						"`verdict entities retract --symbol %d` and apply again: the new row supersedes the old one and both stay replayable at their own pins",
					row.ISIN, row.SymbolID, row.EntityID, diff, row.SymbolID)
			}
			out.Skipped = append(out.Skipped, row)
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO symbol_links
			    (symbol_id, entity_id, reason, boundary, predecessor, evidence, roster_sha, seeded_by, note)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			row.SymbolID, row.EntityID, row.Reason, row.Boundary, row.PredecessorID,
			row.Evidence, digest, seededBy, nullable(row.Note)); err != nil {
			return nil, fmt.Errorf("apply: %s -> %s: %w", row.PredecessorSIN, row.ISIN, err)
		}
		out.Rows = append(out.Rows, row)
	}
	if dryRun {
		return out, tx.Rollback(ctx)
	}
	return out, tx.Commit(ctx)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// plan turns roster lines into the rows the store will hold: it resolves every
// ISIN to its symbol_id and every chain to its root.
//
// entity_id is the chain ROOT's symbol_id and never changes afterwards, so
// extending a chain later is one INSERT pointing the new successor at the
// existing entity_id, with no re-pointing of anything. Resolution is one hop
// and never transitive: A -> B -> C is two rows, both carrying entity_id = A.
func plan(ctx context.Context, pool *pgxpool.Pool, r *Roster) ([]LinkRow, error) {
	ids, err := resolveISINs(ctx, pool, r)
	if err != nil {
		return nil, err
	}
	predecessorOf := map[string]string{} // successor ISIN -> predecessor ISIN
	for _, l := range r.Links {
		predecessorOf[l.Successor] = l.Predecessor
	}
	var rows []LinkRow
	for _, l := range r.Links {
		root := l.Predecessor
		// The roster is already known to be a set of paths (G5), so this walk
		// terminates and lands on the one member with no predecessor.
		for {
			prev, ok := predecessorOf[root]
			if !ok {
				break
			}
			root = prev
		}
		boundary, err := parseDay(l.EffectiveFrom)
		if err != nil {
			return nil, err
		}
		evidence, err := json.Marshal(map[string]any{
			"gates":              l.Gates,
			"evidence_notgating": l.EvidenceNotGating,
			"ticker_at_boundary": l.TickerAtBoundary,
			"ratified_by":        l.RatifiedBy,
			"ratified_at":        l.RatifiedAt,
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, LinkRow{
			SymbolID:       ids[l.Successor],
			EntityID:       ids[root],
			PredecessorID:  ids[l.Predecessor],
			Boundary:       boundary,
			Reason:         reasonOf(l),
			Note:           l.Note,
			Evidence:       evidence,
			ISIN:           l.Successor,
			PredecessorSIN: l.Predecessor,
			EntityISIN:     root,
		})
	}
	// Root-first, so a chain is written in the order it happened. The
	// flatness trigger accepts either order -- every row points at a root
	// that has no row of its own -- but a human reading the table wants the
	// boundaries in order.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].EntityID != rows[j].EntityID {
			return rows[i].EntityID < rows[j].EntityID
		}
		return rows[i].Boundary.Before(rows[j].Boundary)
	})
	return rows, nil
}

// resolveISINs maps every ISIN the roster names to its symbol_id.
//
// An ISIN the store has never registered is an error rather than a skip. It
// means the roster is describing a company this store has no bars for, so the
// link would be unverifiable by `entities check` and invisible to every read
// -- a row asserting something nothing can contradict.
func resolveISINs(ctx context.Context, pool *pgxpool.Pool, r *Roster) (map[string]int64, error) {
	want := make([]string, 0, 2*len(r.Links))
	for _, l := range r.Links {
		want = append(want, l.Predecessor, l.Successor)
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT isin, symbol_id FROM symbols WHERE isin = ANY($1)`, want)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]int64{}
	for rows.Next() {
		var isin string
		var id int64
		if err := rows.Scan(&isin, &id); err != nil {
			return nil, err
		}
		if prev, ok := ids[isin]; ok && prev != id {
			return nil, fmt.Errorf("apply: %s holds two symbol_ids (%d and %d); the registry is broken and no link can be written over it", isin, prev, id)
		}
		ids[isin] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var missing []string
	for _, isin := range want {
		if _, ok := ids[isin]; !ok {
			missing = append(missing, isin)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("apply: %d ISIN(s) in the roster are not registered in this store: %v", len(missing), missing)
	}
	return ids, nil
}

// querier is the part of pgx a read here needs, so the same helpers serve a
// pool and a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// linkState is one symbol's latest row: what it resolves to now, and the two
// columns that say HOW it was linked. A re-apply has to compare those rather
// than assume them, because entity_id alone cannot tell a repeat run from a
// correction.
type linkState struct {
	EntityID    int64
	Boundary    *time.Time
	Predecessor *int64
}

// differsFrom reports, in words, how the stored row disagrees with the row
// this roster would write for the same symbol in the same entity. An empty
// string means the two say the same thing and the row is a genuine repeat.
func (s linkState) differsFrom(row LinkRow) string {
	var diffs []string
	stored := "NULL"
	if s.Boundary != nil {
		stored = s.Boundary.Format("2006-01-02")
	}
	if s.Boundary == nil || !s.Boundary.Equal(row.Boundary) {
		diffs = append(diffs, fmt.Sprintf("the stored boundary is %s and the roster says %s",
			stored, row.Boundary.Format("2006-01-02")))
	}
	if s.Predecessor == nil || *s.Predecessor != row.PredecessorID {
		got := "NULL"
		if s.Predecessor != nil {
			got = strconv.FormatInt(*s.Predecessor, 10)
		}
		diffs = append(diffs, fmt.Sprintf("the stored predecessor is %s and the roster says %d (%s)",
			got, row.PredecessorID, row.PredecessorSIN))
	}
	return joinAnd(diffs)
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; and "
		}
		out += p
	}
	return out
}

// currentMap is the map as resolved now: the latest row per symbol. It is
// deliberately unpinned, because a write decides what the map will be from
// now on and a past pin cannot be affected by it.
func currentMap(ctx context.Context, q querier) (map[int64]linkState, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (symbol_id) symbol_id, entity_id, boundary, predecessor
		FROM symbol_links ORDER BY symbol_id, ingested_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]linkState{}
	for rows.Next() {
		var symbolID int64
		var st linkState
		if err := rows.Scan(&symbolID, &st.EntityID, &st.Boundary, &st.Predecessor); err != nil {
			return nil, err
		}
		out[symbolID] = st
	}
	return out, rows.Err()
}

// checkEntityNames runs migration 0004's two flatness guards BEFORE the
// insert, so the operator gets an error describing what they actually did.
//
// The trigger's own message for the first case is "re-point every member or
// none", which is advice design 4.1 and 10 both forbid taking. The case it
// fires on is real and reachable from the design's own ops flow: a backfill
// that extends the archive backwards makes `propose` emit a predecessor OLDER
// than the current chain root, `plan` then recomputes the root and the roster
// asks for the entity to be RENAMED. Design 4.1 says such a predecessor
// "joins by pointing at the existing entity_id rather than renaming the
// entity" -- but the row it would need is (symbol P, entity A) with no
// predecessor to record, and design 4.2's CHECK requires predecessor IS NOT
// NULL on every non-retraction row. The two are not compatible, so this
// refuses and says so rather than improvising a row shape the design does not
// define. See docs/DESIGN.md, Stage 2.
func checkEntityNames(rows []LinkRow, current map[int64]linkState) error {
	membersOf := map[int64][]int64{}
	for symbolID, st := range current {
		if st.EntityID != symbolID {
			membersOf[st.EntityID] = append(membersOf[st.EntityID], symbolID)
		}
	}
	for _, list := range membersOf {
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
	}
	for _, row := range rows {
		if kids := membersOf[row.SymbolID]; len(kids) > 0 {
			return fmt.Errorf(
				"apply: %s (symbol %d) is already the ENTITY of %v, and this roster makes it a member of entity %d (%s) instead -- "+
					"an older predecessor discovered later renames the entity, which design 4.1 forbids ('it joins by pointing at the existing entity_id'), "+
					"and the row design 4.1 wants has no predecessor to record while design 4.2's CHECK requires one. "+
					"Refusing rather than writing half a re-point: this needs a design decision, not a roster edit",
				row.ISIN, row.SymbolID, kids, row.EntityID, row.EntityISIN)
		}
		if st, ok := current[row.EntityID]; ok && st.EntityID != row.EntityID {
			return fmt.Errorf(
				"apply: this roster names symbol %d (%s) as the entity of %s (symbol %d), but %d itself resolves to %d; "+
					"the map is one hop and never transitive, so a chain must be rooted at a member with no link of its own",
				row.EntityID, row.EntityISIN, row.ISIN, row.SymbolID, row.EntityID, st.EntityID)
		}
	}
	return nil
}

// retractedSymbols reports which of these symbols carry a retraction row.
//
// A retracted pair is regenerated into next month's roster by `propose`,
// which reads bars and not symbol_links, so `apply` must not re-link one
// quietly: the skip branch cannot see it either, because a retraction row
// sets entity_id = symbol_id and the row looks like a first link. Design 8.4
// requires the re-link to remain POSSIBLE, so this warns rather than refuses.
func retractedSymbols(ctx context.Context, q querier, rows []LinkRow) (map[int64]bool, error) {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.SymbolID)
	}
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	res, err := q.Query(ctx,
		`SELECT DISTINCT symbol_id FROM symbol_links WHERE reason = 'retraction' AND symbol_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	for res.Next() {
		var id int64
		if err := res.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, res.Err()
}

// RetractedMember is one member dissolved out of an entity.
type RetractedMember struct {
	SymbolID int64
	ISIN     string
	WasIn    int64
}

// RetractResult reports what retract undid.
//
// Remaining is how many linked members the entity still has afterwards, and
// SymbolScoped says which verb ran. Together they are what the CLI needs to
// describe what happened without claiming an entity was dissolved when it was
// not.
type RetractResult struct {
	EntityID     int64
	Members      []RetractedMember
	Remaining    int
	SymbolScoped bool
}

// Retract dissolves an ENTITY: it resolves ref through the map, then writes
// one retraction row for every member whose entity_id differs from its own
// symbol_id. From that moment those symbols resolve to themselves again and
// the merge is gone; every answer given while the link stood stays
// permanently replayable at its own timestamp, because the bad row is not
// deleted -- it cannot be, and it should not be.
//
// It is entity-scoped, and that is a correction. A symbol-scoped retract had
// two failure modes, both real: naming a LINKED symbol was rejected outright
// by the flatness trigger, and naming the ROOT succeeded and did nothing --
// the row landed, the CLI reported success, and the map was unchanged because
// the member's own latest row still pointed at the root. An operator told
// "entity 326 is contaminated" does the obvious thing, gets a confirmation,
// and the merge stands: a wrong answer with no error in the one code path
// whose entire job is to correct a wrong answer.
//
// symbolScoped keeps `--symbol` available for the single-member case and
// refuses, naming the other members, when the named symbol is a root that
// still has members hanging off it.
func Retract(ctx context.Context, pool *pgxpool.Pool, ref, note, seededBy string, symbolScoped bool) (*RetractResult, error) {
	if seededBy == "" {
		seededBy = SeededBy
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", linkLockKey); err != nil {
		return nil, err
	}

	target, err := resolveRef(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	current, err := currentMap(ctx, tx)
	if err != nil {
		return nil, err
	}
	entityID := target
	if got, ok := current[target]; ok {
		entityID = got.EntityID
	}
	var members []int64
	for symbolID, st := range current {
		if st.EntityID == entityID && symbolID != st.EntityID {
			members = append(members, symbolID)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	linked := len(members)

	if symbolScoped {
		if entityID == target && len(members) > 0 {
			return nil, fmt.Errorf(
				"retract: symbol %d is the ROOT of entity %d, whose other members are %v; retracting the root alone inserts a row, reports success and leaves the merge standing -- re-run with --entity %d to dissolve the entity",
				target, entityID, members, entityID)
		}
		if entityID == target {
			return nil, fmt.Errorf("retract: symbol %d is not linked to anything; there is nothing to undo", target)
		}
		// Design 4.6 keeps --symbol "as an alias only for the single-member
		// case", and this is that sentence rather than half of it. Detaching
		// ONE member of a multi-member entity leaves its siblings pointed at
		// the old root: one company split across two entity ids, with a hole
		// in the middle of the survivor's history that belongs to the member
		// just detached. Nothing downstream catches it -- the resulting map
		// is flat (I1 silent), disjoint (I2 silent) and same-issuer (I3
		// silent) -- so it is the same silent split as revision 1's defect,
		// arriving from the member side.
		if len(members) > 1 {
			return nil, fmt.Errorf(
				"retract: symbol %d is one of %d linked members of entity %d (%v); detaching it alone would leave the others pointed at %d and split one company across two entity ids, which nothing downstream detects -- re-run with --entity %d to dissolve the entity",
				target, len(members), entityID, members, entityID, entityID)
		}
		members = []int64{target}
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("retract: entity %d has no linked members; there is nothing to undo", entityID)
	}

	isins, err := isinsOf(ctx, tx, append(members, entityID))
	if err != nil {
		return nil, err
	}
	res := &RetractResult{EntityID: entityID, SymbolScoped: symbolScoped, Remaining: linked - len(members)}
	for _, symbolID := range members {
		evidence, err := json.Marshal(map[string]any{
			"retracted_from": entityID,
			"member_isin":    isins[symbolID],
			"entity_isin":    isins[entityID],
			"scope":          scopeName(symbolScoped),
		})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO symbol_links (symbol_id, entity_id, reason, evidence, seeded_by, note)
			VALUES ($1, $1, 'retraction', $2, $3, $4)`,
			symbolID, evidence, seededBy, nullable(note)); err != nil {
			return nil, fmt.Errorf("retract: member %d: %w", symbolID, err)
		}
		res.Members = append(res.Members, RetractedMember{SymbolID: symbolID, ISIN: isins[symbolID], WasIn: entityID})
	}
	return res, tx.Commit(ctx)
}

func scopeName(symbolScoped bool) string {
	if symbolScoped {
		return "symbol"
	}
	return "entity"
}

// resolveRef accepts a symbol_id or an ISIN, because an operator holding a
// universe row has the ISIN and an operator holding an error message has the
// id.
func resolveRef(ctx context.Context, q querier, ref string) (int64, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return id, nil
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT symbol_id FROM symbols WHERE isin = $1`, ref)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch len(ids) {
	case 0:
		return 0, fmt.Errorf("no symbol registered for %q", ref)
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("%q holds %d symbol_ids %v; name one by id", ref, len(ids), ids)
	}
}

func isinsOf(ctx context.Context, q querier, ids []int64) (map[int64]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT symbol_id, isin FROM symbols WHERE symbol_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var isin string
		if err := rows.Scan(&id, &isin); err != nil {
			return nil, err
		}
		out[id] = isin
	}
	return out, rows.Err()
}
