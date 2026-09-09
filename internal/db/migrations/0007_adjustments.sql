-- +goose Up
-- +goose StatementBegin

-- adjustments: corporate actions that change the share count without changing
-- what the holder owns -- bonus issues, splits, and consolidations.
--
-- The design listed this table from the start and framed it as a SIGNAL
-- problem: without it a return computed across a split reads -90%. That framing
-- understated it badly. The larger failure is in the BOOK, not the signal: a
-- backtest holding Reliance through its 2024 bonus kept the same share count at
-- half the price and lost half the position to arithmetic. Reliance did it
-- twice, HDFC Bank in 2025, Infosys in 2018, HCL Tech in 2015 -- every name a
-- liquid Indian portfolio holds. Measured across the archive, 15 to 83 such
-- events a year occur in liquid names, so a fifty-name book met several
-- annually and each one destroyed about a percent of it.
--
-- ratio is the share multiplier on ex_date: 2.0 for a 1:1 bonus, 5.0 for a 1:5
-- split, 0.5 for a 1:2 consolidation. A holder's quantity is multiplied by it
-- and the price divides by it, so the position's value is unchanged -- which is
-- the whole point, and exactly what the unadjusted archive fails to express.
--
-- Insert-only and versioned like everything else. A corrected factor is a new
-- row, and a run pinned before the correction still sees what it saw.
CREATE TABLE adjustments (
    entity_id   bigint        NOT NULL,
    ex_date     date          NOT NULL,
    source      text          NOT NULL,
    ratio       numeric(18,8) NOT NULL CHECK (ratio > 0 AND ratio <> 1),
    -- evidence records how the ratio was derived, so a wrong one can be traced
    -- rather than argued about. Today that is the two sources' closes on the
    -- day before and the day of.
    evidence    jsonb         NOT NULL,
    method      text          NOT NULL,
    ingested_at timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (entity_id, ex_date, source, ingested_at)
);

CREATE INDEX adjustments_date_idx ON adjustments (ex_date);
CREATE INDEX adjustments_entity_idx ON adjustments (entity_id, ex_date);

CREATE TRIGGER adjustments_insert_only BEFORE UPDATE OR DELETE ON adjustments
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE VIEW adjustments_latest AS
    SELECT DISTINCT ON (entity_id, ex_date, source) *
    FROM adjustments
    ORDER BY entity_id, ex_date, source, ingested_at DESC;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW adjustments_latest;
DROP TABLE adjustments;
-- +goose StatementEnd
