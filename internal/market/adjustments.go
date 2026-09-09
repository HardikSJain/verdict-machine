package market

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Corporate actions, derived rather than guessed.
//
// The archive of record is unadjusted: nse-bhavcopy prints the price actually
// traded, so a 1:1 bonus halves it overnight. eod2 is adjusted in place: it
// restates history so today's series is continuous. Neither alone gives the
// share multiplier, and together they give it exactly.
//
//	factor(d) = bhavcopy_close(d) / eod2_close(d)
//
// is the cumulative adjustment still ahead of date d, so it is flat between
// corporate actions and steps at each one. Reliance measured on the live store:
// 4.0 before 2017-09-07, 2.0 from then, 1.0 from 2024-10-28. Two 1:1 bonuses,
// both recovered to four decimal places, from data already ingested.
//
// This is why eod2 is in the project at all. The design kept it as a
// survivorship cross-check; it turns out to carry the adjustment history that
// the source of record cannot express.
//
// **What it cannot cover, and the coverage report says so on every run:** eod2
// is survivor-only. A company that delisted has no adjusted series, so no factor
// can be derived for it, and any corporate action it had before dying stays
// invisible. That is a real hole and it is reported rather than smoothed over.

// SimpleFraction reports whether r is within tol of p/q for small integers, and
// returns the fraction it matched.
//
// This is what separates a share-count action from a dividend, and the two must
// be separated. eod2's adjusted series accounts for BOTH, so its ratio against
// the unadjusted archive steps at a bonus and also at every dividend. A bonus
// changes what you hold: one share becomes two. A dividend does not -- it pays
// cash -- and crediting it as extra shares would quietly turn every strategy
// here into a total-return strategy while the benchmark stayed a price index,
// which is exactly the flattering direction this project declared it would
// avoid.
//
// Share actions are ratios of small whole numbers: 2 for a 1:1 bonus, 5 for a
// face-value split of ten rupees into two, 1.5 for 1:2, 0.5 for a
// consolidation. Dividend adjustments are small and arbitrary -- 1.021, 0.977 --
// and match no such fraction. The classification is a heuristic and it is
// reported as one: anything unmatched is counted and left out rather than
// guessed at.
// MinShareActionStep is the smallest change in share count this detector will
// accept, and it is a floor on the whole method rather than a tuning knob.
//
// A first pass allowed any ratio of integers up to 20 and classified 1,411
// steps as share actions. Most were dividends: 1.056 is within a percent of
// 19/18, and with four hundred candidate fractions almost any number matches
// one. The fix is not a tighter tolerance but a smaller candidate set and a
// floor, because the two populations are separated by SIZE. Indian corporate
// actions cluster at 2 (1:1 bonus), 5 and 10 (face-value splits), 1.5 (1:2) and
// 1.1 (1:10); dividend adjustments sit between 1% and 6% and match nothing
// real. Ten percent excludes every dividend and the rarest bonuses with it,
// which is the safe direction: a missed 1:20 bonus costs 5% of one position,
// while a dividend mistaken for a share issue would silently convert every
// strategy here into a total-return one measured against a price index.
const MinShareActionStep = 0.09

func SimpleFraction(r, tol float64) (p, q int, ok bool) {
	if r <= 0 || math.Abs(r-1) < MinShareActionStep {
		return 0, 0, false
	}
	// Denominators stay small because real actions are ratios of small whole
	// numbers. A bonus of a-for-b gives (a+b)/b; a face-value split of X into Y
	// gives X/Y. Neither produces nineteen-eighteenths.
	for den := 1; den <= 10; den++ {
		for num := 1; num <= 20; num++ {
			if num == den {
				continue
			}
			if math.Abs(r/(float64(num)/float64(den))-1) <= tol {
				return num, den, true
			}
		}
	}
	return 0, 0, false
}

// Adjustment is one corporate action.
type Adjustment struct {
	EntityID int64
	ExDate   time.Time
	Ratio    float64 // share multiplier: 2.0 for a 1:1 bonus
	Method   string
	Evidence map[string]any
}

// DetectionSummary counts what the detector saw, so the discarded majority is
// visible rather than silently dropped.
type DetectionSummary struct {
	Steps        int // ratio moves above the floor
	ShareActions int // of those, ones matching a simple fraction
	Unclassified int // the rest, almost all dividend adjustments
	// Uncorroborated counts fractions the PRICE did not confirm. A share action
	// of ratio R must be accompanied by the price falling to about 1/R on the
	// same session; when it is not, the two sources disagree about which day
	// the action landed and the ratio cannot be trusted.
	Uncorroborated int
}

// DetectAdjustments finds every step in the two sources' ratio.
//
// minStep is how far the factor must move to count. It exists because the two
// sources round differently and their ratio wobbles in the fourth decimal;
// without a floor, that noise would be written as thousands of 1.0001 corporate
// actions. Anything above it is a real change in share count -- the smallest
// genuine action is a 1:20 bonus at 1.05.
// PriceCorroborationTol is how far the price move may sit from the one the
// ratio implies before the action is rejected.
//
// It is the second, independent confirmation, and it exists because the first
// pass without it was wrong 183 times in 892. eod2 does not always apply an
// adjustment on the same session the unadjusted archive prints the fall --
// KRITIKA shows a ratio of 3 on a day its price rose 1.5%, SKYGOLD a ratio of
// 10 against a 71% fall where 90% was required. Every one of those would have
// multiplied a share count with no corresponding price drop, which is value
// manufactured out of a date mismatch.
//
// Rejecting them errs toward the OLD bug -- a missed action understates the
// book, as the whole project did before this layer existed -- and that is the
// safe direction. Inventing shares is not.
const PriceCorroborationTol = 0.10

func (s *Store) DetectAdjustments(ctx context.Context, minStep, fracTol float64, asOfIngest time.Time) ([]Adjustment, DetectionSummary, error) {
	var sum DetectionSummary
	if minStep <= 0 || minStep >= 1 {
		return nil, sum, fmt.Errorf("adjustments: minStep must be in (0,1), got %g", minStep)
	}
	// The two series are joined by entity AND by ticker, and the candidates are
	// unioned. Either key alone has a blind spot, and they are not the same one.
	//
	// eod2 files a company's whole history under ONE symbol -- its current ISIN
	// -- while the unadjusted archive splits that history across every ISIN the
	// company ever had. So the factor series has a seam wherever an ISIN was
	// reissued, and the corporate action lives exactly on that seam.
	//
	// Joining by ENTITY closes the seam when the roster has linked the two
	// ISINs. It fails when the roster has not: Yes Bank holds three ISINs across
	// two entities because the roster linked two and quarantined the third, so
	// its 1:5 split fell across the seam and vanished.
	//
	// Joining by TICKER closes the seam without needing the roster -- but only
	// while the company keeps its name. Ami Organics reissued its ISIN at a 1:2
	// split in April 2025 and later renamed to Acutaas Chemicals. Symbols are
	// labelled by their latest ticker, so the pre-split unadjusted bars read
	// AMIORG and the adjusted series reads ACUTAAS. They never met, the split
	// was invisible, and a held position lost half its value to arithmetic. That
	// was found by a backtest reporting a 51% one-session fall it could not
	// explain, not by this query.
	//
	// So: both keys, union the steps, prefer the entity's answer where both
	// fire. The union cannot manufacture an action, because every candidate --
	// whichever key produced it -- must still match a simple share fraction AND
	// be corroborated by the price move below. That is also what makes the
	// ticker key safe against ticker REUSE, one label meaning two companies at
	// different times: a reused ticker produces a ratio the price does not
	// confirm, and is dropped.
	//
	// The action is attributed to whichever entity holds the bar on the ex-date,
	// because that is what the engine's book is keyed on.
	rows, err := s.pool.Query(ctx, `WITH `+entityMapCTE("$1")+`,
		lbl AS (
			SELECT DISTINCT ON (symbol_id) symbol_id, ticker
			FROM symbols WHERE ingested_at <= $1
			ORDER BY symbol_id, ingested_at DESC
		),
		keyed AS (
			SELECT m.symbol_id, m.entity_id, k.key
			FROM entity_map m
			JOIN lbl l ON l.symbol_id = m.symbol_id
			CROSS JOIN LATERAL (VALUES
				('e:' || m.entity_id::text), ('t:' || l.ticker)
			) k(key)
		),
		bhav AS (
			SELECT DISTINCT ON (k.key, b.date) k.key, b.date,
			       b.close::float8 AS close, k.entity_id
			FROM bars b
			JOIN keyed k ON k.symbol_id = b.symbol_id
			WHERE b.source = $2 AND b.ingested_at <= $1 AND b.close > 0
			ORDER BY k.key, b.date, b.ingested_at DESC
		),
		adj AS (
			SELECT DISTINCT ON (k.key, b.date) k.key, b.date, b.close::float8 AS close
			FROM bars b
			JOIN keyed k ON k.symbol_id = b.symbol_id
			WHERE b.source = $3 AND b.ingested_at <= $1 AND b.close > 0
			ORDER BY k.key, b.date, b.ingested_at DESC
		),
		f AS (
			SELECT bhav.key, bhav.date, bhav.entity_id,
			       bhav.close AS bhav_close, bhav.close / adj.close AS factor
			FROM bhav JOIN adj USING (key, date)
		),
		stepped AS (
			SELECT key, date, entity_id, bhav_close, factor,
			       lag(factor)     OVER (PARTITION BY key ORDER BY date) AS prev_factor,
			       lag(bhav_close) OVER (PARTITION BY key ORDER BY date) AS prev_bhav,
			       lag(date)       OVER (PARTITION BY key ORDER BY date) AS prev_date
			FROM f
		)
		SELECT DISTINCT ON (entity_id, date)
		       entity_id, date, prev_date, prev_factor / factor AS ratio,
		       prev_bhav, bhav_close, prev_factor, factor
		FROM stepped
		WHERE prev_factor IS NOT NULL
		  AND abs(prev_factor / factor - 1) > $4
		ORDER BY entity_id, date, key`,
		asOfIngest, SourceBhavcopy, SourceEod2, minStep)
	if err != nil {
		return nil, sum, fmt.Errorf("adjustments: detect: %w", err)
	}
	defer rows.Close()

	var out []Adjustment
	for rows.Next() {
		var a Adjustment
		var exDate, prevDate time.Time
		var prevBhav, bhav, prevFactor, factor float64
		if err := rows.Scan(&a.EntityID, &exDate, &prevDate, &a.Ratio,
			&prevBhav, &bhav, &prevFactor, &factor); err != nil {
			return nil, sum, err
		}
		sum.Steps++
		num, den, ok := SimpleFraction(a.Ratio, fracTol)
		if !ok {
			sum.Unclassified++
			continue
		}
		// Snap to the exact fraction. The measured ratio carries both sources
		// rounding; the corporate action itself is exact, and a share count
		// must be too.
		exact := float64(num) / float64(den)
		// The price must corroborate. A 1:1 bonus doubles the share count and
		// halves the price, so (1 + priceChange) * ratio must land near 1.
		priceChange := bhav/prevBhav - 1
		if math.Abs((1+priceChange)*exact-1) > PriceCorroborationTol {
			sum.Uncorroborated++
			continue
		}
		sum.ShareActions++
		a.Ratio = exact
		a.ExDate = Day(exDate.Year(), exDate.Month(), exDate.Day())
		a.Method = "eod2-ratio-step"
		a.Evidence = map[string]any{
			"prev_date":            prevDate.Format(time.DateOnly),
			"prev_bhavcopy_close":  prevBhav,
			"bhavcopy_close":       bhav,
			"factor_before":        prevFactor,
			"factor_after":         factor,
			"measured_ratio":       prevFactor / factor,
			"fraction":             fmt.Sprintf("%d/%d", num, den),
			"implied_price_change": bhav/prevBhav - 1,
		}
		out = append(out, a)
	}
	return out, sum, rows.Err()
}

// InsertAdjustments writes them, skipping any whose ratio already matches the
// latest version held.
func (s *Store) InsertAdjustments(ctx context.Context, source string, adjs []Adjustment) (int, error) {
	var written int
	for _, a := range adjs {
		var existing *float64
		err := s.pool.QueryRow(ctx, `
			SELECT ratio::float8 FROM adjustments
			WHERE entity_id = $1 AND ex_date = $2 AND source = $3
			ORDER BY ingested_at DESC LIMIT 1`, a.EntityID, a.ExDate, source).Scan(&existing)
		if err != nil && err.Error() != "no rows in result set" {
			return written, fmt.Errorf("adjustments: reading current version: %w", err)
		}
		if existing != nil && math.Abs(*existing-a.Ratio) < 1e-8 {
			continue
		}
		ev, err := json.Marshal(a.Evidence)
		if err != nil {
			return written, err
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO adjustments (entity_id, ex_date, source, ratio, evidence, method)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			a.EntityID, a.ExDate, source, a.Ratio, ev, a.Method); err != nil {
			return written, fmt.Errorf("adjustments: insert: %w", err)
		}
		written++
	}
	return written, nil
}

// AdjustmentsOn returns every corporate action effective on a session, keyed by
// entity, as the store knew them at asOfIngest.
//
// The engine calls it once per session and multiplies held share counts by the
// ratio. That is the whole repair: a holder of a company that issues a 1:1
// bonus ends the day with twice the shares at half the price and exactly the
// same money, which is what actually happens and what the unadjusted archive
// cannot say.
func (s *Store) AdjustmentsOn(ctx context.Context, date, asOfIngest time.Time) (map[int64]float64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (entity_id) entity_id, ratio::float8
		FROM adjustments
		WHERE ex_date = $1 AND ingested_at <= $2
		ORDER BY entity_id, ingested_at DESC`, date, asOfIngest)
	if err != nil {
		return nil, fmt.Errorf("adjustments on %s: %w", date.Format(time.DateOnly), err)
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var r float64
		if err := rows.Scan(&id, &r); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// AdjustmentCoverage reports how much of the archive can have factors derived
// at all: entities with both an unadjusted and an adjusted series, against
// those with only the unadjusted one.
//
// The gap is eod2's survivorship. A company that delisted has no adjusted
// series and therefore no recoverable corporate actions, so a backtest holding
// it still loses a bonus to arithmetic. Reporting the number is the only honest
// option available until a corporate-actions feed is ingested.
func (s *Store) AdjustmentCoverage(ctx context.Context, asOfIngest time.Time) (both, bhavOnly int, err error) {
	err = s.pool.QueryRow(ctx, `WITH `+entityMapCTE("$1")+`,
		b AS (SELECT DISTINCT m.entity_id FROM bars x JOIN entity_map m ON m.symbol_id = x.symbol_id
		      WHERE x.source = $2 AND x.ingested_at <= $1),
		e AS (SELECT DISTINCT m.entity_id FROM bars x JOIN entity_map m ON m.symbol_id = x.symbol_id
		      WHERE x.source = $3 AND x.ingested_at <= $1)
		SELECT count(*) FILTER (WHERE e.entity_id IS NOT NULL),
		       count(*) FILTER (WHERE e.entity_id IS NULL)
		FROM b LEFT JOIN e USING (entity_id)`,
		asOfIngest, SourceBhavcopy, SourceEod2).Scan(&both, &bhavOnly)
	return both, bhavOnly, err
}
