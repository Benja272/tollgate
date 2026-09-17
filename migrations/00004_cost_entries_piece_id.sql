-- +goose Up
-- piece_id ties a job's spend to the downstream content piece it served.
-- Nullable and outside the natural key: two jobs may share one piece_id
-- (ADR-0006 §9).
ALTER TABLE cost_entries ADD COLUMN piece_id TEXT;
CREATE INDEX cost_entries_piece_id_idx ON cost_entries (piece_id) WHERE piece_id IS NOT NULL;

-- run_id is the Temporal run id of the execution that wrote the row. The
-- same job_id can be executed more than once (re-running a piece to compare
-- models), and every execution bills; without run_id in the natural key the
-- second execution's row hit ON CONFLICT DO NOTHING and its spend was lost.
-- Retries within one execution still collide and stay idempotent.
-- NOT NULL with an empty default: a NULL would never conflict, which would
-- silently turn off idempotency for any writer that leaves it unset.
ALTER TABLE cost_entries ADD COLUMN run_id TEXT NOT NULL DEFAULT '';
DROP INDEX cost_entries_natural_key;
CREATE UNIQUE INDEX cost_entries_natural_key
    ON cost_entries (job_id, run_id, phase, actor, attempt);

-- +goose Down
-- Restoring the old key fails if two executions of one job both recorded
-- the same (phase, actor, attempt): collapsing them would drop spend, so
-- the rollback refuses instead of deleting rows.
DROP INDEX cost_entries_natural_key;
CREATE UNIQUE INDEX cost_entries_natural_key
    ON cost_entries (job_id, phase, actor, attempt);
ALTER TABLE cost_entries DROP COLUMN run_id;

DROP INDEX cost_entries_piece_id_idx;
ALTER TABLE cost_entries DROP COLUMN piece_id;
