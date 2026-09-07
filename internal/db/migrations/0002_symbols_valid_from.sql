-- +goose Up
-- symbols recorded when a ticker version was *ingested*, never which session
-- date it was observed for, so nothing tied a ticker to a point in market
-- time. Reads resolved the label with `ORDER BY ingested_at DESC LIMIT 1`,
-- which returns whichever version happened to land last: running an eod2
-- ingest (every year of a ticker's history mapped to today's ticker) while a
-- backfill walks historical files made a symbol's ticker oscillate, and a
-- 2015 bar was then labelled with whatever name was written most recently.
--
-- valid_from is the session date the ticker was observed for -- the bar
-- file's own date. It adds the as-of-calendar axis the reads need, alongside
-- the as-of-ingest axis the audit trail needs.
--
-- Rows written before this migration have no observed date to recover, so
-- they take 0001-01-01: every one of them applies to every bar date, and ties
-- among them still break on ingested_at DESC, which is exactly how they were
-- read before. The default is then dropped so every new version has to state
-- the date it was observed for. (Consequence worth knowing before running this
-- against a live database: a `verdict` binary built before this migration
-- inserts symbols rows without valid_from and will fail to register a new
-- ISIN once it is applied, so apply it between runs, not during one.)
ALTER TABLE symbols ADD COLUMN valid_from date NOT NULL DEFAULT DATE '0001-01-01';
ALTER TABLE symbols ALTER COLUMN valid_from DROP DEFAULT;
CREATE INDEX symbols_symbol_id_valid_from_idx ON symbols (symbol_id, valid_from DESC, ingested_at DESC);

-- +goose Down
DROP INDEX symbols_symbol_id_valid_from_idx;
ALTER TABLE symbols DROP COLUMN valid_from;
