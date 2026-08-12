package ports

import (
	"context"

	"github.com/Benja272/tollgate/internal/gate"
)

// JudgeRequest is what one judge evaluates: the diff an agent produced, the
// ticket it was asked to solve, and the rubric to score against.
type JudgeRequest struct {
	Diff   string
	Ticket string
	Rubric gate.Rubric
}

// Judgment is a verdict plus what producing it cost: judging spends money
// like any other LLM call, and the ledger models the job's FULL cost.
type Judgment struct {
	Verdict gate.Verdict
	CostUSD float64
	Usage   TokenUsage
}

// Judge scores one attempt against a rubric, independently of any other
// judge (ADR-0003).
type Judge interface {
	Judge(ctx context.Context, req JudgeRequest) (Judgment, error)
}
