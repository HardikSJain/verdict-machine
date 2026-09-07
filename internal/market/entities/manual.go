package entities

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The hand-written line, and how it gets into the file.

// EvaluatePair measures one named pair against the store exactly as the
// generator would, without generating anything. It is what `entities link`
// uses to fill in a hand-written line's evidence.
//
// The gates are RECORDED and not enforced here: a manual line exists because
// no gate admitted it, and refusing to write down what the gates said would
// leave the riskiest row in the table as the only one carrying no measurement
// at all. The reviewer of that line sees the same numbers as for a generated
// one, plus the failures.
func EvaluatePair(ctx context.Context, pool *pgxpool.Pool, predISIN, succISIN string, asOfIngest time.Time) (*Candidate, error) {
	if asOfIngest.IsZero() {
		asOfIngest = time.Now()
	}
	facts, err := loadSymbolFacts(ctx, pool, asOfIngest)
	if err != nil {
		return nil, err
	}
	byISIN := map[string]*symbolFacts{}
	for _, f := range facts {
		byISIN[f.ISIN] = f
	}
	p, ok := byISIN[predISIN]
	if !ok {
		return nil, fmt.Errorf("link: %s has no %s bars in this store", predISIN, "nse-bhavcopy")
	}
	s, ok := byISIN[succISIN]
	if !ok {
		return nil, fmt.Errorf("link: %s has no %s bars in this store", succISIN, "nse-bhavcopy")
	}
	sessions, err := loadSessions(ctx, pool, asOfIngest)
	if err != nil {
		return nil, err
	}
	settled, lastFetch, err := loadSettlement(ctx, pool, asOfIngest)
	if err != nil {
		return nil, err
	}
	holes, _ := archiveHoles(sessions[0], sessions[len(sessions)-1], settled, lastFetch)

	c := newCandidate("manual", p, s)
	if err := measureOverlaps(ctx, pool, asOfIngest, []*Candidate{c}); err != nil {
		return nil, err
	}
	gate(c, sessions, settled, len(holes))
	return c, nil
}

// AppendManual adds one hand-decided line to a roster file and rewrites it.
// It writes nothing to the store: identity is decided in git, and this verb
// only edits the file the reviewer will read.
func AppendManual(path string, c *Candidate, ratifiedBy, note string) (*Roster, error) {
	r := &Roster{Version: RosterVersion}
	if _, err := os.Stat(path); err == nil {
		loaded, _, err := Load(path)
		if err != nil {
			return nil, err
		}
		r = loaded
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for _, l := range r.Links {
		if l.Predecessor == c.Predecessor && l.Successor == c.Successor {
			return nil, fmt.Errorf("link: %s -> %s is already in %s", c.Predecessor, c.Successor, path)
		}
	}
	r.Links = append(r.Links, Link{
		Reason:            ReasonManual,
		Predecessor:       c.Predecessor,
		Successor:         c.Successor,
		TickerAtBoundary:  c.TickerAfter,
		EffectiveFrom:     c.SuccessorSpn[0],
		Gates:             c.Gates,
		EvidenceNotGating: c.EvidenceNotGating,
		RatifiedBy:        ratifiedBy,
		RatifiedAt:        time.Now().UTC().Format("2006-01-02"),
		Note:              note,
	})
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := r.Marshal()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return nil, err
	}
	return r, nil
}
