-- +goose Up
-- piece_id ties a job's spend to the downstream content piece it served.
-- Nullable and outside every existing index: two jobs may share one
-- piece_id, and it must never affect the (job_id, phase, actor, attempt)
-- idempotency key (ADR-0006 D13).
ALTER TABLE cost_entries ADD COLUMN piece_id TEXT;
CREATE INDEX cost_entries_piece_id_idx ON cost_entries (piece_id) WHERE piece_id IS NOT NULL;

-- +goose Down
DROP INDEX cost_entries_piece_id_idx;
ALTER TABLE cost_entries DROP COLUMN piece_id;
