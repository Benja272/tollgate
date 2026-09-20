// Package postgres implements tollgate's persistence ports over PostgreSQL
// (ADR-0004).
package postgres

import (
	"context"
	"errors"
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

// ErrMissingRunID rejects a new row without a run id: it would collide
// with every other execution of its job and silently drop their spend.
// Only rows written before run_id existed carry ”. This is the backstop:
// an entry journaled by a pre-run-id worker gets its run id filled from the
// execution that scheduled the write, in engine.Activities.RecordCosts.
var ErrMissingRunID = errors.New("ledger: cost entry has no run id")

// RecordCosts writes a batch atomically. ON CONFLICT DO NOTHING over the
// natural key (job, run, phase, actor, attempt) makes retries within one
// execution idempotent — an at-least-once redelivery never double-counts
// money — while a second execution of the same job keeps its own rows.
func (l *Ledger) RecordCosts(ctx context.Context, entries []ports.CostEntry) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ledger: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, e := range entries {
		if e.RunID == "" {
			return fmt.Errorf("%w: %s/%s/%s", ErrMissingRunID, e.JobID, e.Phase, e.Actor)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO cost_entries
			   (job_id, run_id, phase, actor, model, input_tokens, output_tokens,
			    cache_read_tokens, cache_creation_tokens, usd, attempt, piece_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''))
			 ON CONFLICT (job_id, run_id, phase, actor, attempt) DO NOTHING`,
			e.JobID, e.RunID, e.Phase, e.Actor, e.Model,
			e.Usage.InputTokens, e.Usage.OutputTokens,
			e.Usage.CacheReadTokens, e.Usage.CacheCreationTokens,
			e.USD, e.Attempt, e.PieceID,
		); err != nil {
			return fmt.Errorf("ledger: insert %s/%s/%s: %w", e.JobID, e.Phase, e.Actor, err)
		}
	}
	return tx.Commit(ctx)
}

// PieceSpend is one model's total spend for a piece, summed across every
// job that shares its piece_id, regardless of job outcome.
type PieceSpend struct {
	Model string
	USD   float64
}

// PerPieceSpend sums cost_entries by model for one piece_id (ADR-0006 §9):
// SUM across every recorded row of every job and every execution, whether
// or not that execution ultimately succeeded — a job that failed after the
// agent ran still contributes its run_agent row.
func (l *Ledger) PerPieceSpend(ctx context.Context, pieceID string) ([]PieceSpend, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT model, COALESCE(SUM(usd), 0) FROM cost_entries
		 WHERE piece_id = $1 GROUP BY model ORDER BY model`, pieceID)
	if err != nil {
		return nil, fmt.Errorf("ledger: per-piece spend for %s: %w", pieceID, err)
	}
	defer rows.Close()

	var out []PieceSpend
	for rows.Next() {
		var s PieceSpend
		if err := rows.Scan(&s.Model, &s.USD); err != nil {
			return nil, fmt.Errorf("ledger: scan per-piece spend for %s: %w", pieceID, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
