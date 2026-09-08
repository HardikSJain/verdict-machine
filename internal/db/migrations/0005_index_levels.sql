-- +goose Up
-- +goose StatementBegin

-- index_levels: NSE's daily index archive, one row per index per session.
--
-- Indices are NOT bars and do not belong in that table. They have no ISIN, no
-- symbol_id and no deliverable quantity; more importantly, `bars` is what the
-- point-in-time universe ranks by turnover, and an index carrying a turnover
-- figure that is the sum of its constituents' would rank above every stock in
-- it. Keeping them apart is what stops that from ever being possible.
--
-- Insert-only and versioned on ingested_at, like everything else here: a
-- correction is a new row, and a query pinned before it still sees what it saw.
-- Version key is (index_code, date, source).
--
-- index_code is the CANONICAL identity and index_name is the name NSE printed
-- that session. They differ because NSE rebranded its whole index family: the
-- Nifty 50 was published as "S&P CNX Nifty" until 2013, "CNX Nifty" until
-- November 2015, and "Nifty 50" since. Keying on the printed name would
-- fragment fourteen years of the same index into three unrelated series --
-- exactly the failure ISIN succession caused for Tata Steel, arriving through a
-- different door. Both columns are stored so the canonical choice stays
-- auditable against what was actually published.
CREATE TABLE index_levels (
    index_code    text          NOT NULL,
    date          date          NOT NULL,
    source        text          NOT NULL,
    index_name    text          NOT NULL,
    open          numeric(14,4),
    high          numeric(14,4),
    low           numeric(14,4),
    close         numeric(14,4) NOT NULL,
    points_change numeric(14,4),
    pct_change    numeric(12,4),
    volume        bigint,
    turnover      numeric(20,2),
    pe            numeric(12,4),
    pb            numeric(12,4),
    div_yield     numeric(12,4),
    content_hash  bytea         NOT NULL,
    ingested_at   timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (index_code, date, source, ingested_at)
);

-- Open, high and low are nullable and close is not, which is what the archive
-- actually provides: 27 of the 165 indices in a 2026 file publish a close and
-- print "-" for the other three, because they are computed once a day rather
-- than continuously. A NOT NULL on open would have rejected a fifth of the file.
COMMENT ON COLUMN index_levels.open IS 'null where NSE printed "-": index computed once daily';

CREATE INDEX index_levels_code_date_idx ON index_levels (index_code, date);
CREATE INDEX index_levels_date_idx ON index_levels (date);

CREATE TRIGGER index_levels_insert_only BEFORE UPDATE OR DELETE ON index_levels
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- index_levels_latest: the current version of every index level.
CREATE VIEW index_levels_latest AS
    SELECT DISTINCT ON (index_code, date, source) *
    FROM index_levels
    ORDER BY index_code, date, source, ingested_at DESC;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW index_levels_latest;
DROP TABLE index_levels;
-- +goose StatementEnd
