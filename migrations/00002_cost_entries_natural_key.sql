-- +goose Up
-- Natural key making ledger writes idempotent: Temporal activities are
-- at-least-once, and a retried INSERT must hit ON CONFLICT DO NOTHING
-- instead of double-counting money.
CREATE UNIQUE INDEX cost_entries_natural_key
    ON cost_entries (job_id, phase, actor, attempt);

-- +goose Down
DROP INDEX cost_entries_natural_key;
