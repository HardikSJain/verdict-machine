package market

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Run is one recorded execution of a strategy.
type Run struct {
	RunID      string
	Mode       string
	Strategy   string
	GitSHA     string
	ConfigHash string
	SnapshotID string
	IngestPin  time.Time
	PeriodFrom time.Time
	PeriodTo   time.Time
	Result     any
	StartedAt  time.Time
	FinishedAt time.Time
}

// NewRunID returns a random uuid v4.
func NewRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// RecordRun writes the row. It is written AFTER the run finishes, in one
// insert, because the table is insert-only: a row claimed at the start and
// updated at the end would need an UPDATE the trigger forbids, and a run that
// crashed would leave a permanent row claiming a result it never produced.
func (s *Store) RecordRun(ctx context.Context, r Run) error {
	var payload []byte
	if r.Result != nil {
		b, err := json.Marshal(r.Result)
		if err != nil {
			return fmt.Errorf("record run: result: %w", err)
		}
		payload = b
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (run_id, mode, strategy, git_sha, config_hash, snapshot_id,
		                  ingest_pin, period_from, period_to, result, started_at, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		r.RunID, r.Mode, r.Strategy, r.GitSHA, r.ConfigHash, r.SnapshotID,
		r.IngestPin, r.PeriodFrom, r.PeriodTo, payload, r.StartedAt, r.FinishedAt)
	if err != nil {
		return fmt.Errorf("record run: %w", err)
	}
	return nil
}

// PriorRuns returns earlier runs sharing a config and snapshot, so a repeat can
// be recognised as a repeat rather than presented as fresh evidence. Spending a
// holdout twice on one hypothesis is the failure this exists to make visible.
func (s *Store) PriorRuns(ctx context.Context, configHash, snapshotID string) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, mode, strategy, started_at, period_from, period_to
		FROM runs WHERE config_hash = $1 AND snapshot_id = $2 ORDER BY started_at`,
		configHash, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("prior runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.RunID, &r.Mode, &r.Strategy, &r.StartedAt, &r.PeriodFrom, &r.PeriodTo); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HoldoutRuns returns every recorded run whose period reached at or past a date.
//
// A sealed holdout is only sealed if somebody can tell whether it has been
// opened. This is that check, and it reads the store rather than a document
// because a document is not evidence about what was run.
func (s *Store) HoldoutRuns(ctx context.Context, holdoutStart time.Time) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, mode, strategy, started_at, period_from, period_to
		FROM runs WHERE period_to >= $1 ORDER BY started_at`, holdoutStart)
	if err != nil {
		return nil, fmt.Errorf("holdout runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.RunID, &r.Mode, &r.Strategy, &r.StartedAt, &r.PeriodFrom, &r.PeriodTo); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
