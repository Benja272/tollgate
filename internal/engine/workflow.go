// Package engine owns the Temporal workflow that drives a tollgate job:
// prepare -> run agent -> judge -> gate -> ship | reject. Workflow code is
// deterministic; every side effect lives in an activity (see Activities).
package engine

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the Temporal task queue tollgate workers poll and clients
// submit jobs to.
const TaskQueue = "tollgate-jobs"

// runAgentMaxAttempts bounds retries of the agent run. The agent is a paid
// external call with no idempotency guarantee, and Temporal's default retry
// policy is unbounded — every extra attempt bills again (ADR-0003 makes
// retry spend an explicit budget, never an accident).
const runAgentMaxAttempts = 2

// JobInput identifies the work a job performs and where it happens. Prompt
// is the ticket text handed to the agent; a richer intake model (issue
// sources, base branches) is still open design.
type JobInput struct {
	JobID     string
	Repo      string
	SourceRef string
	Prompt    string
}

// JobStatus is the terminal state of a job.
type JobStatus string

const (
	StatusShipped  JobStatus = "shipped"
	StatusRejected JobStatus = "rejected"
)

// JobResult is what a completed workflow reports back. CostUSD is the
// agent-reported estimate for the run (the full per-phase ledger is a
// separate milestone).
type JobResult struct {
	Status  JobStatus
	PRURL   string
	CostUSD float64
}

// Workspace is the isolated checkout a job runs in.
type Workspace struct {
	Path string
}

// RunAgentInput carries what one agent run needs: where to work and what to
// do.
type RunAgentInput struct {
	Workspace Workspace
	Prompt    string
}

// AgentResult is the adapter-normalized outcome of one agent run.
type AgentResult struct {
	CostUSD float64
	Output  string
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

	// HeartbeatTimeout lets the server detect a dead worker mid-agent-run in
	// seconds instead of waiting out StartToCloseTimeout; adapters must
	// heartbeat at least every couple of seconds while the agent runs.
	agentCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    5 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: runAgentMaxAttempts},
	})
	var agent AgentResult
	if err := workflow.ExecuteActivity(agentCtx, acts.RunAgent, RunAgentInput{Workspace: ws, Prompt: in.Prompt}).Get(agentCtx, &agent); err != nil {
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
		return JobResult{Status: StatusRejected, CostUSD: agent.CostUSD}, nil
	}

	var shipped ShipResult
	if err := workflow.ExecuteActivity(ctx, acts.Ship, ws).Get(ctx, &shipped); err != nil {
		return JobResult{}, err
	}

	return JobResult{Status: StatusShipped, PRURL: shipped.PRURL, CostUSD: agent.CostUSD}, nil
}
