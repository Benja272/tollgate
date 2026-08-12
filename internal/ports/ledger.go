package ports

import "context"

// CostEntry is one ledger row: what one actor spent in one phase of one
// job attempt. USD is a client-side estimate; the ledger still stores it
// exactly (NUMERIC) so aggregates add up.
type CostEntry struct {
	JobID        string
	Phase        string
	Actor        string
	Model        string
	InputTokens  int64
	OutputTokens int64
	USD          float64
	Attempt      int32
}

// LedgerStore persists cost entries. Implementations must be idempotent on
// the natural key (job, phase, actor, attempt): Temporal activities are
// at-least-once, so a retried write must never double-count money.
type LedgerStore interface {
	RecordCosts(ctx context.Context, entries []CostEntry) error
}
