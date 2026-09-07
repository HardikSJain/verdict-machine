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
	out := &ApplyResult{DryRun: dryRun}
	for _, row := range rows {
		if got, ok := current[row.SymbolID]; ok && got != row.SymbolID {
			if got == row.EntityID {
				out.Skipped = append(out.Skipped, row)
				continue
			}
			return nil, fmt.Errorf(
				"apply: %s (symbol %d) already resolves to entity %d, and this roster puts it in entity %d; "+
					"retract the existing link first -- re-pointing a member silently would move a company between entities with nothing recording that the old answer was withdrawn",
				row.ISIN, row.SymbolID, got, row.EntityID)
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

// currentMap is the map as resolved now: the latest row per symbol. It is
// deliberately unpinned, because a write decides what the map will be from
// now on and a past pin cannot be affected by it.
func currentMap(ctx context.Context, q querier) (map[int64]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (symbol_id) symbol_id, entity_id
		FROM symbol_links ORDER BY symbol_id, ingested_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var symbolID, entityID int64
		if err := rows.Scan(&symbolID, &entityID); err != nil {
			return nil, err
		}
		out[symbolID] = entityID
	}
	return out, rows.Err()
}

// RetractedMember is one member dissolved out of an entity.
type RetractedMember struct {
	SymbolID int64
	ISIN     string
	WasIn    int64
}

// RetractResult reports what retract undid.
type RetractResult struct {
	EntityID int64
	Members  []RetractedMember
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
		entityID = got
	}
	var members []int64
	for symbolID, e := range current {
		if e == entityID && symbolID != e {
			members = append(members, symbolID)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })

	if symbolScoped {
		if entityID == target && len(members) > 0 {
			return nil, fmt.Errorf(
				"retract: symbol %d is the ROOT of entity %d, whose other members are %v; retracting the root alone inserts a row, reports success and leaves the merge standing -- re-run with --entity %d to dissolve the entity",
				target, entityID, members, entityID)
		}
		if entityID == target {
			return nil, fmt.Errorf("retract: symbol %d is not linked to anything; there is nothing to undo", target)
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
	res := &RetractResult{EntityID: entityID}
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
