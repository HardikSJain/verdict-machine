package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// Snapshot identity: what every versioned table held at one ingest pin.
//
// The point is narrow and worth stating exactly. Every table below is
// insert-only and versioned on ingested_at, so "the state at a pin" is a
// well-defined set of rows: the latest version of each key with
// ingested_at <= pin. Hashing that set gives a value that changes if and only if
// something the run could see changed. A backfill that adds a session, a
// corrected bhavcopy, one more roster line -- each moves the snapshot, and a
// replay that produces a different number under a different snapshot has
// explained itself rather than raised an alarm.
//
// **The digest is hierarchical, and that is a performance decision with a
// correctness consequence worth knowing.** Hashing 14 million bars by
// concatenating them would build a multi-gigabyte string in Postgres. Instead
// each date is digested separately in SQL, and Go hashes the ordered list of
// per-date digests. The result is still a function of every row, and it is still
// order-dependent, but two different row sets that happened to collide inside
// one date's md5 would collide overall. md5 is used for the inner level because
// the threat here is accident rather than an adversary constructing collisions
// against his own audit trail; the outer digest is sha256.

// TableDigest is one table's contribution, kept so a report can say which table
// moved rather than only that something did.
type TableDigest struct {
	Table  string
	Rows   int64
	Digest string
}

// SnapshotID returns a hex sha256 over every versioned table at the pin, plus
// the per-table digests behind it.
func (s *Store) SnapshotID(ctx context.Context, asOfIngest time.Time) (string, []TableDigest, error) {
	var out []TableDigest

	// bars: DISTINCT ON walks the primary key (symbol_id, date, source,
	// ingested_at) in its own order, so the latest-version selection is an
	// index scan rather than a sort of fourteen million rows.
	barsDigest, barsRows, err := s.groupedDigest(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (symbol_id, date, source) symbol_id, date, source, content_hash
			FROM bars WHERE ingested_at <= $1
			ORDER BY symbol_id, date, source, ingested_at DESC
		)
		SELECT date::text,
		       md5(string_agg(symbol_id::text || '|' || source || '|' || encode(content_hash, 'hex'),
		                      ',' ORDER BY symbol_id, source)),
		       count(*)
		FROM latest GROUP BY date ORDER BY date`, asOfIngest)
	if err != nil {
		return "", nil, fmt.Errorf("snapshot: bars: %w", err)
	}
	out = append(out, TableDigest{"bars", barsRows, barsDigest})

	idxDigest, idxRows, err := s.groupedDigest(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (index_code, date, source) index_code, date, source, content_hash
			FROM index_levels WHERE ingested_at <= $1
			ORDER BY index_code, date, source, ingested_at DESC
		)
		SELECT date::text,
		       md5(string_agg(index_code || '|' || source || '|' || encode(content_hash, 'hex'),
		                      ',' ORDER BY index_code, source)),
		       count(*)
		FROM latest GROUP BY date ORDER BY date`, asOfIngest)
	if err != nil {
		return "", nil, fmt.Errorf("snapshot: index_levels: %w", err)
	}
	out = append(out, TableDigest{"index_levels", idxRows, idxDigest})

	// symbols and symbol_links are small enough to digest in one pass.
	symDigest, symRows, err := s.flatDigest(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (symbol_id) symbol_id, isin, ticker, valid_from
			FROM symbols WHERE ingested_at <= $1
			ORDER BY symbol_id, ingested_at DESC
		)
		SELECT md5(string_agg(symbol_id::text || '|' || isin || '|' || ticker || '|' || valid_from::text,
		                      ',' ORDER BY symbol_id)), count(*)
		FROM latest`, asOfIngest)
	if err != nil {
		return "", nil, fmt.Errorf("snapshot: symbols: %w", err)
	}
	out = append(out, TableDigest{"symbols", symRows, symDigest})

	// symbol_links is in this set because the link map decides every answer as
	// surely as a price does: without it, a hand-written link inserted with a
	// backdated ingested_at would change an old run's result with nothing able
	// to notice.
	linkDigest, linkRows, err := s.flatDigest(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (symbol_id) symbol_id, entity_id, reason,
			       coalesce(boundary::text, '') AS bnd, coalesce(predecessor::text, '') AS pred,
			       encode(roster_sha, 'hex') AS rs
			FROM symbol_links WHERE ingested_at <= $1
			ORDER BY symbol_id, ingested_at DESC
		)
		SELECT md5(string_agg(symbol_id::text || '|' || entity_id::text || '|' || reason || '|' ||
		                      bnd || '|' || pred || '|' || rs, ',' ORDER BY symbol_id)),
		       count(*)
		FROM latest`, asOfIngest)
	if err != nil {
		return "", nil, fmt.Errorf("snapshot: symbol_links: %w", err)
	}
	out = append(out, TableDigest{"symbol_links", linkRows, linkDigest})

	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	h := sha256.New()
	for _, t := range out {
		fmt.Fprintf(h, "%s|%d|%s\n", t.Table, t.Rows, t.Digest)
	}
	return hex.EncodeToString(h.Sum(nil)), out, nil
}

// groupedDigest hashes a per-group digest stream into one value without
// materialising the whole table as a string anywhere.
func (s *Store) groupedDigest(ctx context.Context, q string, asOfIngest time.Time) (string, int64, error) {
	rows, err := s.pool.Query(ctx, q, asOfIngest)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	h := sha256.New()
	var total int64
	for rows.Next() {
		var key, digest string
		var n int64
		if err := rows.Scan(&key, &digest, &n); err != nil {
			return "", 0, err
		}
		fmt.Fprintf(h, "%s:%s\n", key, digest)
		total += n
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

func (s *Store) flatDigest(ctx context.Context, q string, asOfIngest time.Time) (string, int64, error) {
	var digest *string
	var n int64
	if err := s.pool.QueryRow(ctx, q, asOfIngest).Scan(&digest, &n); err != nil {
		return "", 0, err
	}
	if digest == nil {
		// An empty table is a legitimate state and must hash to something
		// stable rather than to whatever a nil produces.
		return "empty", 0, nil
	}
	return *digest, n, nil
}

// RosterDigests returns the distinct roster shas the applied entity map was
// built from, hex encoded and sorted.
//
// It reads the STORE rather than the roster.json on disk, because what decides a
// run's answers is the map that was applied, not a file that may since have been
// regenerated. That distinction is the whole reason config_hash carries it.
func (s *Store) RosterDigests(ctx context.Context, asOfIngest time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT encode(roster_sha, 'hex') FROM symbol_links
		WHERE ingested_at <= $1 ORDER BY 1`, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("roster digests: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
