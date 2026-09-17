package ports

import "context"

// CostEntry is one ledger row: what one actor spent in one phase of one
// job attempt. USD is a client-side estimate; the ledger still stores it
// exactly (NUMERIC) so aggregates add up.
type CostEntry struct {
	JobID   string
	RunID   string
	Phase   string
	Actor   string
	Model   string
	Usage   TokenUsage
	USD     float64
	Attempt int32
	// PieceID ties a job's spend to the downstream content piece it served,
	// when the caller supplies one (ArtifactJobWorkflow). It is nullable and
	// deliberately outside the natural key: two jobs can share a piece_id,
	// and it must never affect idempotency (ADR-0006 §9).
	PieceID string
}

// LedgerStore persists cost entries. Implementations must be idempotent on
// the natural key (job, run, phase, actor, attempt): Temporal activities are
// at-least-once, so a retried write must never double-count money.
type LedgerStore interface {
	RecordCosts(ctx context.Context, entries []CostEntry) error
}
