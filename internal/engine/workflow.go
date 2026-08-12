// Package engine owns the Temporal workflow that drives a tollgate job:
// prepare -> run agent -> judge -> gate -> ship | reject. Workflow code is
// deterministic; every side effect lives in an activity (see Activities).
package engine

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
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

	// RubricPath selects the rubric file; empty means defaultRubricPath.
	RubricPath string
	// JudgeModels selects the judge models, one activity each; empty means
	// defaultJudgeModels.
	JudgeModels []string
}

// Gate defaults. Resolved inside the workflow, so they are pinned by the
// journal like any other deterministic value.
var defaultJudgeModels = []string{"haiku", "sonnet"}

const defaultRubricPath = "rubrics/default.yaml"

// JobStatus is the terminal state of a job.
type JobStatus string

const (
	StatusShipped  JobStatus = "shipped"
	StatusRejected JobStatus = "rejected"
)

// JobResult is what a completed workflow reports back. CostUSD aggregates
// the agent run and every judgment; it is a client-side estimate (the
// itemized per-actor ledger is a separate milestone).
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
	Usage   ports.TokenUsage
	Output  string
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

	if err := workflow.ExecuteActivity(ctx, acts.RecordCosts, []ports.CostEntry{{
		JobID: in.JobID, Phase: "run_agent", Actor: "agent",
		Usage: agent.Usage, USD: agent.CostUSD, Attempt: 1,
	}}).Get(ctx, nil); err != nil {
		return JobResult{}, err
	}

	rubricPath := in.RubricPath
	if rubricPath == "" {
		rubricPath = defaultRubricPath
	}
	var rubric gate.Rubric
	if err := workflow.ExecuteActivity(ctx, acts.LoadRubric, rubricPath).Get(ctx, &rubric); err != nil {
		return JobResult{}, err
	}

	// One activity per judge, all in flight at once: each judgment is
	// journaled, retried, and costed independently (ADR-0003).
	models := in.JudgeModels
	if len(models) == 0 {
		models = defaultJudgeModels
	}
	futures := make([]workflow.Future, len(models))
	for i, model := range models {
		futures[i] = workflow.ExecuteActivity(ctx, acts.JudgeOne, JudgeInput{
			Model:  model,
			Change: agent.Output,
			Ticket: in.Prompt,
			Rubric: rubric,
		})
	}
	verdicts := make([]gate.Verdict, len(models))
	judgeEntries := make([]ports.CostEntry, len(models))
	totalCost := agent.CostUSD
	for i, f := range futures {
		var judgment ports.Judgment
		if err := f.Get(ctx, &judgment); err != nil {
			return JobResult{}, err
		}
		verdicts[i] = judgment.Verdict
		totalCost += judgment.CostUSD
		judgeEntries[i] = ports.CostEntry{
			JobID: in.JobID, Phase: "judge", Actor: "judge:" + models[i],
			Model: models[i], Usage: judgment.Usage, USD: judgment.CostUSD, Attempt: 1,
		}
	}
	if err := workflow.ExecuteActivity(ctx, acts.RecordCosts, judgeEntries).Get(ctx, nil); err != nil {
		return JobResult{}, err
	}

	var decision gate.Decision
	if err := workflow.ExecuteActivity(ctx, acts.DecideGate, DecideInput{Rubric: rubric, Verdicts: verdicts}).Get(ctx, &decision); err != nil {
		return JobResult{}, err
	}

	if decision.Outcome != gate.OutcomePass {
		return JobResult{Status: StatusRejected, CostUSD: totalCost}, nil
	}

	var shipped ShipResult
	if err := workflow.ExecuteActivity(ctx, acts.Ship, ws).Get(ctx, &shipped); err != nil {
		return JobResult{}, err
	}

	return JobResult{Status: StatusShipped, PRURL: shipped.PRURL, CostUSD: totalCost}, nil
}
