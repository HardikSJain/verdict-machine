-- +goose Up
-- bars holds one row-set per source, and `source` is what names the price
-- convention each of them carries: eod2 bars are split/bonus adjusted in
-- place, nse-bhavcopy bars are unadjusted. A future read-time adjustment
-- layer therefore has to key on this column, and a value outside the
-- allow-list would be a row-set with no stated convention at all.
--
-- Until now the allow-list lived only in two comments and in a Go-side guard
-- in UniverseAsOf, while ingest_log.status beside it has carried a CHECK from
-- the start. bars is insert-only, so a typo'd source is not a row that can be
-- corrected later; it is a row that stays.
ALTER TABLE bars ADD CONSTRAINT bars_source_check CHECK (source IN ('eod2', 'nse-bhavcopy'));

-- +goose Down
ALTER TABLE bars DROP CONSTRAINT bars_source_check;
