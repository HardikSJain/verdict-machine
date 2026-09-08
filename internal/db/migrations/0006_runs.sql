-- +goose Up
-- +goose StatementBegin

-- runs: one row per backtest or evening, recording everything needed to
-- reproduce it.
--
-- The design calls this a hard prerequisite for M1 and warns it is easy to skip
-- because nothing fails loudly without it. What fails quietly is this: a result
-- published today cannot be checked tomorrow. A later backfill, a revised
-- bhavcopy, one more roster line, or an edited parameter all change the answer,
-- and without a recorded row there is no way to tell a genuine change of mind
-- from a silent change of data.
--
-- Three columns carry that between them:
--
--   snapshot_id  the state of every versioned table the run could see, at its
--                own ingest pin. Changes if any row it read changed or was
--                added.
--   config_hash  the parameters, including the digest of the entity roster in
--                force. Changes if the rule changed.
--   git_sha      the code.
--
-- A replay that reproduces all three and gets a different number has found a
-- bug. A replay that cannot reproduce them is not a replay.
CREATE TABLE runs (
    run_id          uuid        PRIMARY KEY,
    mode            text        NOT NULL,
    strategy        text        NOT NULL,
    git_sha         text        NOT NULL,
    config_hash     text        NOT NULL,
    snapshot_id     text        NOT NULL,
    ingest_pin      timestamptz NOT NULL,
    period_from     date        NOT NULL,
    period_to       date        NOT NULL,
    -- ledger_head_seq is the last event the run could see. The ledger arrives
    -- in M2; the column exists now so a run written today is not missing a
    -- field a replay written later expects.
    ledger_head_seq bigint,
    account_id      text,
    result          jsonb,
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);

CREATE INDEX runs_started_idx ON runs (started_at DESC);
CREATE INDEX runs_config_idx ON runs (config_hash, snapshot_id);

-- Insert-only like everything else that records what was believed when. A run
-- row is a claim about a moment; editing one rewrites history rather than
-- correcting it. finished_at is set by the same INSERT, not by a later UPDATE.
CREATE TRIGGER runs_insert_only BEFORE UPDATE OR DELETE ON runs
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE runs;
-- +goose StatementEnd
