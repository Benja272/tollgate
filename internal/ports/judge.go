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

// Judge scores one attempt against a rubric, independently of any other
// judge (ADR-0003).
type Judge interface {
	Judge(ctx context.Context, req JudgeRequest) (gate.Verdict, error)
}
