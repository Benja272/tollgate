// Package engine owns the Temporal workflow that drives a tollgate job:
// prepare -> run agent -> judge -> gate -> ship | reject. Workflow code is
// deterministic; every side effect lives in an activity (see Activities).
package engine

import (
	"context"
	"time"

	"go.temporal.io/sdk/workflow"
)

// JobInput identifies the work a job performs and where it happens.
type JobInput struct {
	JobID     string
	Repo      string
	SourceRef string
}

// JobStatus is the terminal state of a job.
type JobStatus string

const (
	StatusShipped  JobStatus = "shipped"
	StatusRejected JobStatus = "rejected"
)

// JobResult is what a completed workflow reports back.
type JobResult struct {
	Status JobStatus
	PRURL  string
}

// Workspace is the isolated checkout a job runs in.
type Workspace struct {
	Path string
}

// AgentResult is the adapter-normalized outcome of one agent run.
type AgentResult struct {
	CostUSD float64
}

// JudgeReport aggregates the verdicts of all judges for one attempt.
type JudgeReport struct{}

// GateOutcome is the gate's verdict over a judged attempt.
type GateOutcome string

const (
	GatePass GateOutcome = "pass"
	GateFail GateOutcome = "fail"
)

// GateDecision is the resolution-policy output for one attempt (ADR-0003).
type GateDecision struct {
	Outcome GateOutcome
}

// ShipResult reports the PR created for a passing job.
type ShipResult struct {
	PRURL string
}

// Activities declares every side-effecting step of the job pipeline.
// Implementations live behind ports; the workflow only sees signatures.
type Activities struct{}

func (a *Activities) Prepare(ctx context.Context, in JobInput) (Workspace, error) {
	panic("not implemented")
}

func (a *Activities) RunAgent(ctx context.Context, ws Workspace) (AgentResult, error) {
	panic("not implemented")
}

func (a *Activities) Judge(ctx context.Context, ws Workspace) (JudgeReport, error) {
	panic("not implemented")
}

func (a *Activities) DecideGate(ctx context.Context, report JudgeReport) (GateDecision, error) {
	panic("not implemented")
}

func (a *Activities) Ship(ctx context.Context, ws Workspace) (ShipResult, error) {
	panic("not implemented")
}

// JobWorkflow orchestrates one job from ticket to PR or rejection.
func JobWorkflow(ctx workflow.Context, in JobInput) (JobResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
	})

	var acts *Activities

	var ws Workspace
	if err := workflow.ExecuteActivity(ctx, acts.Prepare, in).Get(ctx, &ws); err != nil {
		return JobResult{}, err
	}

	var agent AgentResult
	if err := workflow.ExecuteActivity(ctx, acts.RunAgent, ws).Get(ctx, &agent); err != nil {
		return JobResult{}, err
	}

	var report JudgeReport
	if err := workflow.ExecuteActivity(ctx, acts.Judge, ws).Get(ctx, &report); err != nil {
		return JobResult{}, err
	}

	var decision GateDecision
	if err := workflow.ExecuteActivity(ctx, acts.DecideGate, report).Get(ctx, &decision); err != nil {
		return JobResult{}, err
	}

	if decision.Outcome != GatePass {
		return JobResult{Status: StatusRejected}, nil
	}

	var shipped ShipResult
	if err := workflow.ExecuteActivity(ctx, acts.Ship, ws).Get(ctx, &shipped); err != nil {
		return JobResult{}, err
	}

	return JobResult{Status: StatusShipped, PRURL: shipped.PRURL}, nil
}
