-- +goose Up
CREATE TABLE cost_entries (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id        TEXT NOT NULL,
    phase         TEXT NOT NULL,
    actor         TEXT NOT NULL,
    model         TEXT NOT NULL DEFAULT '',
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    usd           NUMERIC(12, 6) NOT NULL,
    attempt       INT NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX cost_entries_job_id_idx ON cost_entries (job_id);
CREATE INDEX cost_entries_actor_idx ON cost_entries (actor);

-- +goose Down
DROP TABLE cost_entries;
