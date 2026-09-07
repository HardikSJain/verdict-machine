package market

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// The canonical entity, and the three CTEs that resolve it.
//
// NSE reissues an ISIN on a face-value split, so one company owns several
// symbol_ids and its price history snaps in half at the split date. bars is
// insert-only and every one of its rows carries the symbol_id assigned at
// insert time, so the repair cannot be a rewrite; it is symbol_links, a small
// overlay from physical symbol_id to canonical entity_id, versioned on the
// same ingested_at axis the rest of the store already uses.
//
// An entity is a FLAT equivalence class named by a bigint drawn from the
// symbol_id space. Resolution is exactly one hop and never transitive: a
// chain A -> B -> C is two rows (B and C) both carrying entity_id = A, which
// is why migration 0004 enforces flatness with a write-time trigger rather
// than trusting the reader to walk. A symbol with no link row is its own
// entity, so the ~3,600 symbols that never changed ISIN need no row at all
// and every one of these CTEs degenerates to today's per-symbol behaviour
// when the table is empty -- which is exactly how Stage 1 deploys.

// entityMapCTE names a CTE entity_map(symbol_id, entity_id) resolving every
// registered symbol as the store knew the mapping at ingestExpr.
//
// The ingested_at bound is the whole point and the easiest thing here to
// drop by accident, because the query still looks right without it: a run
// pinned before the seed must resolve every symbol to itself forever, or the
// project's replay promise is gone.
func entityMapCTE(ingestExpr string) string {
	return `entity_map AS (
		SELECT s.symbol_id, COALESCE(l.entity_id, s.symbol_id) AS entity_id
		FROM (SELECT DISTINCT symbol_id FROM symbols WHERE ingested_at <= ` + ingestExpr + `) s
		LEFT JOIN LATERAL (
			SELECT entity_id FROM symbol_links
			WHERE symbol_id = s.symbol_id AND ingested_at <= ` + ingestExpr + `
			ORDER BY ingested_at DESC LIMIT 1
		) l ON true
	)`
}

// entityMemberCTE names a CTE entity_member(entity_id, symbol_id): one row
// per entity, naming the MEMBER in force on dateExpr, chosen from the
// boundary intervals in symbol_links as the store knew them at ingestExpr.
//
// This is the only new identity rule in the design, and it reads `boundary`,
// which is why boundary is NOT NULL for every non-retraction row. The
// alternative -- widening symbolLabelLateral's candidate set from one
// symbol_id to all of an entity's members and ordering on symbols.valid_from
// -- was tried and is wrong on live data in the majority case: migration
// 0002 backfilled 5,172 of 5,816 symbols rows with the sentinel
// valid_from = 0001-01-01, so for 3,745 of 4,100 symbol_ids every member ties
// on every date and the winner is decided by which member the backfill
// happened to register last. Measured over a five-date grid it picked the
// member actually holding that session's bar for 86/291, 56/314, 111/315,
// 195/393 and 160/423 entities, handing a present-day caller ICICIBANK under
// a 2014 ISIN with no error and fragments = 1. The interval rule below scores
// 291/291, 314/314, 315/315, 393/393, 423/423.
//
// The ordering reads: prefer a member whose boundary has already arrived on
// dateExpr (the root has no link row, hence NULL boundary, hence -infinity
// via the COALESCE, so it always qualifies); among those take the latest
// boundary; m.symbol_id is a total-order tiebreak that never runs on a
// well-formed entity, because the seeder's gates make the spans totally
// ordered and the primary key makes the boundary unique per member. A
// retraction row carries boundary IS NULL and so falls back to root-like
// behaviour, which is right: a retracted member is its own entity again.
func entityMemberCTE(dateExpr, ingestExpr string) string {
	return `entity_member AS (
		SELECT DISTINCT ON (m.entity_id) m.entity_id, m.symbol_id
		FROM entity_map m
		LEFT JOIN LATERAL (
			SELECT boundary FROM symbol_links
			WHERE symbol_id = m.symbol_id AND ingested_at <= ` + ingestExpr + `
			ORDER BY ingested_at DESC LIMIT 1
		) k ON true
		ORDER BY m.entity_id,
		         (COALESCE(k.boundary, DATE '0001-01-01') <= ` + dateExpr + `) DESC,
		         k.boundary DESC NULLS LAST,
		         m.symbol_id
	)`
}

// entityLabelCTE names a CTE entity_label(entity_id, symbol_id, isin,
// ticker). It adds no ordering of its own: it calls symbolLabelLateral --
// unchanged, and the same call site the per-symbol reads use -- on the single
// symbol_id entity_member already chose. The per-entity rule and the
// per-symbol rule are therefore not two strings held equal by a test; they
// are one function.
//
// For a symbol with no link row the whole construction collapses to today's
// query character for character: entity_map returns the identity,
// entity_member returns the symbol itself, and the lateral is the one already
// in store.go. That is what TestEntityLabelMatchesSymbolLabelForUnlinkedSymbols
// pins -- and, as that test says at length, it is not evidence about the
// linked path.
func entityLabelCTE(dateExpr, ingestExpr string) string {
	return `entity_label AS (
		SELECT em.entity_id, em.symbol_id, sy.isin, sy.ticker
		FROM entity_member em
		JOIN LATERAL ` + symbolLabelLateral("em.symbol_id", dateExpr, ingestExpr) + ` sy ON true
	)`
}

// EntityBoundary is one succession inside an entity: the date the successor's
// first nse-bhavcopy session fell on, and the two physical symbols either
// side of it.
type EntityBoundary struct {
	Date            time.Time
	PredecessorID   int64
	SuccessorID     int64
	PredecessorISIN string
	SuccessorISIN   string
}

// EntityBoundaries returns every succession boundary inside each entity, as
// the store knew the map at asOfIngest, keyed by entity id and ordered by
// date.
//
// M1's signal layer must call it. After the entity change a company's series
// is continuous in IDENTITY but not in LEVEL: nse-bhavcopy prices are
// unadjusted, so a return computed across a boundary is wrong by the split
// factor, and the factor is not recoverable from the boundary itself (at the
// TATASTEEL boundary the ex-split session falls one session BEFORE the ISIN
// changes, so a ratio taken at the boundary reads 100.35 -> 107.60 and
// concludes "no split", wrong by 10x). Until the read-time adjustments layer
// exists, the only safe use of this is to REFUSE to compute such a return.
func (s *Store) EntityBoundaries(ctx context.Context, entityIDs []int64, asOfIngest time.Time) (map[int64][]EntityBoundary, error) {
	out := map[int64][]EntityBoundary{}
	if len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH k AS (
			SELECT DISTINCT ON (symbol_id) symbol_id, entity_id, boundary, predecessor
			FROM symbol_links
			WHERE ingested_at <= $2
			ORDER BY symbol_id, ingested_at DESC
		)
		SELECT k.entity_id, k.boundary, k.predecessor, k.symbol_id, p.isin, c.isin
		FROM k
		JOIN LATERAL (
			SELECT isin FROM symbols
			WHERE symbol_id = k.predecessor AND ingested_at <= $2
			ORDER BY ingested_at DESC LIMIT 1
		) p ON true
		JOIN LATERAL (
			SELECT isin FROM symbols
			WHERE symbol_id = k.symbol_id AND ingested_at <= $2
			ORDER BY ingested_at DESC LIMIT 1
		) c ON true
		WHERE k.entity_id = ANY($1) AND k.boundary IS NOT NULL
		ORDER BY k.entity_id, k.boundary, k.symbol_id`, entityIDs, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("entity boundaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entityID int64
		var b EntityBoundary
		var d time.Time
		if err := rows.Scan(&entityID, &d, &b.PredecessorID, &b.SuccessorID, &b.PredecessorISIN, &b.SuccessorISIN); err != nil {
			return nil, err
		}
		b.Date = Day(d.Year(), d.Month(), d.Day())
		out[entityID] = append(out[entityID], b)
	}
	return out, rows.Err()
}

// Violation is one failed entity invariant.
type Violation struct {
	Kind     string // "flatness" | "overlap" | "issuer"
	EntityID int64
	Detail   string
}

// Advisory reports whether a violation is informational rather than a defect.
// I3 (issuer agreement) is advisory by design: it is a tautology on the set
// the seeder's own gates admit, so it can only ever fire on a hand-written
// 'manual' line, where a mismatched issuer prefix is a question and not
// necessarily an answer. I1 and I2 are not advisory.
func (v Violation) Advisory() bool { return v.Kind == "issuer" }

// CheckEntityInvariants runs the three entity invariants as the store knew
// the map at asOfIngest.
//
//	I1 flatness      -- no entity_id is itself a symbol_id resolving to a
//	                    different entity_id. Migration 0004's insert trigger
//	                    prevents this; I1 catches a row that got in another
//	                    way, such as a restored dump or a hand-written INSERT
//	                    made with the trigger disabled.
//	I2 disjointness  -- within one source, no two members of one entity hold a
//	                    bar on the same date. This is the catastrophic false
//	                    positive: two genuinely different companies merged in
//	                    a store that cannot delete. Reported per source,
//	                    because eod2 files a company's whole history under
//	                    today's ISIN and so says something different from
//	                    nse-bhavcopy when it fires.
//	I3 issuer        -- every member of an INE entity shares left(isin, 9),
//	                    the country code plus the NSDL issuer code. Advisory.
//
// What it CANNOT do, stated here so nobody later reads it as a safety net it
// is not: a wrong-but-DISJOINT merge -- a reverse-merger shell, a freed
// ticker reused by a different company -- is undetectable from inside the
// store. I2 fires only on date overlap, which the seeder's gates already
// forbid at seed time, and I3 is a tautology on the set those gates admit.
// The gates and one human review pass are the entire defence against the
// outcome this design names as the worst available.
//
// Design §4.6 also gives `verdict entities check` a second half: reporting
// unlinked succession candidates that appeared since the last seed. That half
// is built on the candidate SQL and the G0-G6 gates, which §9 places in Stage
// 2 with the seeder; it is deliberately absent here rather than half-built.
func (s *Store) CheckEntityInvariants(ctx context.Context, asOfIngest time.Time) ([]Violation, error) {
	var out []Violation

	// I1. The map as currently resolved, then the same map joined to itself:
	// a row whose entity_id is a symbol that resolves somewhere else.
	rows, err := s.pool.Query(ctx, `
		WITH m AS (
			SELECT DISTINCT ON (symbol_id) symbol_id, entity_id
			FROM symbol_links WHERE ingested_at <= $1
			ORDER BY symbol_id, ingested_at DESC
		)
		SELECT a.symbol_id, a.entity_id, b.entity_id
		FROM m a JOIN m b ON b.symbol_id = a.entity_id
		WHERE a.entity_id <> a.symbol_id AND b.entity_id <> a.entity_id
		ORDER BY a.symbol_id`, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("entity invariants (I1): %w", err)
	}
	for rows.Next() {
		var symbolID, entityID, resolved int64
		if err := rows.Scan(&symbolID, &entityID, &resolved); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Violation{Kind: "flatness", EntityID: entityID, Detail: fmt.Sprintf(
			"symbol %d points at entity %d, but %d itself resolves to %d; the map is one hop, never transitive, so this entity is silently split in two",
			symbolID, entityID, entityID, resolved)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// I2. Only entities with more than one member can overlap, so the scan is
	// restricted to symbols that a link row actually touches -- a few hundred
	// rows rather than the whole 13.8M-row bars table.
	rows, err = s.pool.Query(ctx, `
		WITH m AS (
			SELECT DISTINCT ON (symbol_id) symbol_id, entity_id
			FROM symbol_links WHERE ingested_at <= $1
			ORDER BY symbol_id, ingested_at DESC
		), members AS (
			SELECT symbol_id, entity_id FROM m WHERE entity_id <> symbol_id
			UNION
			SELECT entity_id, entity_id FROM m WHERE entity_id <> symbol_id
		)
		SELECT me.entity_id, b.source, b.date, count(DISTINCT b.symbol_id)::int
		FROM bars b JOIN members me ON me.symbol_id = b.symbol_id
		WHERE b.ingested_at <= $1
		GROUP BY me.entity_id, b.source, b.date
		HAVING count(DISTINCT b.symbol_id) > 1
		ORDER BY me.entity_id, b.source, b.date`, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("entity invariants (I2): %w", err)
	}
	for rows.Next() {
		var entityID int64
		var source string
		var d time.Time
		var members int
		if err := rows.Scan(&entityID, &source, &d, &members); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Violation{Kind: "overlap", EntityID: entityID, Detail: fmt.Sprintf(
			"%d members hold a %s bar on %s; two members of one entity cannot trade the same session, so the link between them merges two different companies",
			members, source, d.Format("2006-01-02"))})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// I3, advisory.
	rows, err = s.pool.Query(ctx, `
		WITH m AS (
			SELECT DISTINCT ON (symbol_id) symbol_id, entity_id
			FROM symbol_links WHERE ingested_at <= $1
			ORDER BY symbol_id, ingested_at DESC
		), members AS (
			SELECT symbol_id, entity_id FROM m WHERE entity_id <> symbol_id
			UNION
			SELECT entity_id, entity_id FROM m WHERE entity_id <> symbol_id
		), labelled AS (
			SELECT me.entity_id, sy.isin
			FROM members me
			JOIN LATERAL (
				SELECT isin FROM symbols
				WHERE symbol_id = me.symbol_id AND ingested_at <= $1
				ORDER BY ingested_at DESC LIMIT 1
			) sy ON true
			WHERE sy.isin LIKE 'INE%'
		)
		SELECT entity_id, string_agg(DISTINCT left(isin, 9), ', ' ORDER BY left(isin, 9))
		FROM labelled
		GROUP BY entity_id
		HAVING count(DISTINCT left(isin, 9)) > 1
		ORDER BY entity_id`, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("entity invariants (I3): %w", err)
	}
	for rows.Next() {
		var entityID int64
		var prefixes string
		if err := rows.Scan(&entityID, &prefixes); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Violation{Kind: "issuer", EntityID: entityID, Detail: fmt.Sprintf(
			"members carry more than one NSDL issuer code (%s); a face-value split keeps the issuer, so this is worth a human look", prefixes)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].EntityID < out[j].EntityID
	})
	return out, nil
}
