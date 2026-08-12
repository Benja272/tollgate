// Package postgres implements tollgate's persistence ports over PostgreSQL
// (ADR-0004).
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Benja272/tollgate/internal/ports"
)

// Ledger persists cost entries into the cost_entries table.
type Ledger struct {
	pool *pgxpool.Pool
}

func NewLedger(pool *pgxpool.Pool) *Ledger {
	return &Ledger{pool: pool}
}

var _ ports.LedgerStore = (*Ledger)(nil)

// RecordCosts writes a batch atomically. ON CONFLICT DO NOTHING over the
// natural key (job, phase, actor, attempt) makes retries idempotent — an
// at-least-once redelivery never double-counts money.
func (l *Ledger) RecordCosts(ctx context.Context, entries []ports.CostEntry) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ledger: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, e := range entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO cost_entries
			   (job_id, phase, actor, model, input_tokens, output_tokens,
			    cache_read_tokens, cache_creation_tokens, usd, attempt)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			 ON CONFLICT (job_id, phase, actor, attempt) DO NOTHING`,
			e.JobID, e.Phase, e.Actor, e.Model,
			e.Usage.InputTokens, e.Usage.OutputTokens,
			e.Usage.CacheReadTokens, e.Usage.CacheCreationTokens,
			e.USD, e.Attempt,
		); err != nil {
			return fmt.Errorf("ledger: insert %s/%s/%s: %w", e.JobID, e.Phase, e.Actor, err)
		}
	}
	return tx.Commit(ctx)
}
