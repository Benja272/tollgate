-- +goose Up
-- Cache token classes dominate agent-invocation cost (a 10-token prompt
-- ships ~40k tokens of session context); without them the ledger cannot
-- explain its own usd column.
ALTER TABLE cost_entries
    ADD COLUMN cache_read_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN cache_creation_tokens BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE cost_entries
    DROP COLUMN cache_read_tokens,
    DROP COLUMN cache_creation_tokens;
