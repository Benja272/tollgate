// Package engine owns the Temporal workflow that drives a tollgate job:
// prepare -> run agent -> judge -> gate -> ship | reject. Workflow code is
// deterministic; every side effect lives in an activity (see Activities).
package engine

import (
	"fmt"
	"strings"
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

// defaultMaxFixAttempts is the fix-loop budget from ADR-0003: after a
// blocking FAIL the findings go back to the agent this many times before the
// job is terminally rejected. Every attempt is paid work, so the bound is
// explicit, not a retry policy's accident.
const defaultMaxFixAttempts = 2

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
	// MaxFixAttempts bounds the repair attempts a failing gate may trigger
	// (ADR-0003). Zero means defaultMaxFixAttempts; negative disables the fix
	// loop, so the first blocking FAIL is terminal.
	MaxFixAttempts int
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

// RunAgentInput carries what one agent run needs: where to work, what to do,
// and which attempt this is. Attempt 1 is the original run; 2..N are fix
// attempts whose Prompt restates the ticket plus the gate findings to repair
// (ADR-0003). Journaling the attempt number keeps every run self-describing
// in the workflow history, which is the audit trail the ledger joins against.
type RunAgentInput struct {
	Workspace Workspace
	Prompt    string
	Attempt   int32
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

	// The rubric is loaded once, before any paid agent run: every attempt of
	// this job must be judged against the same version (ADR-0003), and a
	// malformed rubric should never cost an agent run.
	rubricPath := in.RubricPath
	if rubricPath == "" {
		rubricPath = defaultRubricPath
	}
	var rubric gate.Rubric
	if err := workflow.ExecuteActivity(ctx, acts.LoadRubric, rubricPath).Get(ctx, &rubric); err != nil {
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

	models := in.JudgeModels
	if len(models) == 0 {
		models = defaultJudgeModels
	}
	maxFixAttempts := in.MaxFixAttempts
	if maxFixAttempts == 0 {
		maxFixAttempts = defaultMaxFixAttempts
	}

	// The fix loop (ADR-0003). Attempt 1 is the original agent run; a
	// blocking FAIL feeds the findings back to the `fixer` actor in the same
	// workspace and re-judges in full under the same rubric version. Nothing
	// is overwritten: each attempt appends its own cost entries, and each
	// gate decision lands in the journal through the DecideGate activity.
	var totalCost float64
	prompt := in.Prompt
	for attempt := int32(1); ; attempt++ {
		actor := "fixer"
		if attempt == 1 {
			actor = "agent"
		}

		var agent AgentResult
		if err := workflow.ExecuteActivity(agentCtx, acts.RunAgent, RunAgentInput{
			Workspace: ws, Prompt: prompt, Attempt: attempt,
		}).Get(agentCtx, &agent); err != nil {
			return JobResult{}, err
		}
		totalCost += agent.CostUSD

		if err := workflow.ExecuteActivity(ctx, acts.RecordCosts, []ports.CostEntry{{
			JobID: in.JobID, Phase: "run_agent", Actor: actor,
			Usage: agent.Usage, USD: agent.CostUSD, Attempt: attempt,
		}}).Get(ctx, nil); err != nil {
			return JobResult{}, err
		}

		// One activity per judge, all in flight at once: each judgment is
		// journaled, retried, and costed independently (ADR-0003).
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
		for i, f := range futures {
			var judgment ports.Judgment
			if err := f.Get(ctx, &judgment); err != nil {
				return JobResult{}, err
			}
			verdicts[i] = judgment.Verdict
			totalCost += judgment.CostUSD
			judgeEntries[i] = ports.CostEntry{
				JobID: in.JobID, Phase: "judge", Actor: "judge:" + models[i],
				Model: models[i], Usage: judgment.Usage, USD: judgment.CostUSD, Attempt: attempt,
			}
		}
		if err := workflow.ExecuteActivity(ctx, acts.RecordCosts, judgeEntries).Get(ctx, nil); err != nil {
			return JobResult{}, err
		}

		var decision gate.Decision
		if err := workflow.ExecuteActivity(ctx, acts.DecideGate, DecideInput{Rubric: rubric, Verdicts: verdicts}).Get(ctx, &decision); err != nil {
			return JobResult{}, err
		}

		if decision.Outcome == gate.OutcomePass {
			var shipped ShipResult
			if err := workflow.ExecuteActivity(ctx, acts.Ship, ws).Get(ctx, &shipped); err != nil {
				return JobResult{}, err
			}
			return JobResult{Status: StatusShipped, PRURL: shipped.PRURL, CostUSD: totalCost}, nil
		}

		// Budget exhausted: the job is terminally rejected with every
		// attempt's cost accounted for.
		if int(attempt) > maxFixAttempts {
			return JobResult{Status: StatusRejected, CostUSD: totalCost}, nil
		}
		prompt = repairPrompt(in.Prompt, rubric, decision, verdicts, attempt+1, int32(maxFixAttempts))
	}
}

// repairPrompt builds the fixer's instruction: the original ticket plus what
// the gate blocked on. It is a pure function of journaled values and walks
// only ordered slices — decision.FailedBlocking is in rubric order, verdicts
// in judge order — never a Go map, whose iteration order would make a replay
// rebuild a different prompt.
func repairPrompt(ticket string, r gate.Rubric, d gate.Decision, verdicts []gate.Verdict, attempt, maxFixAttempts int32) string {
	var b strings.Builder
	b.WriteString(ticket)
	b.WriteString("\n\n--- QUALITY GATE FEEDBACK ---\n\n")
	fmt.Fprintf(&b, "Your previous attempt was rejected by the quality gate (rubric %s, policy %s).\n",
		d.RubricVersion, d.Policy)
	fmt.Fprintf(&b, "This is repair attempt %d of %d; after that the job is rejected for good.\n\n",
		attempt-1, maxFixAttempts)

	b.WriteString("Blocking axes that failed:\n")
	for _, axis := range d.FailedBlocking {
		minScore := 0
		for _, ax := range r.Axes {
			if ax.Name == axis {
				minScore = ax.MinScore
				break
			}
		}
		fmt.Fprintf(&b, "- %s (min score %d):", axis, minScore)
		for _, v := range verdicts {
			fmt.Fprintf(&b, " %s scored %d;", v.Judge, v.Scores[axis])
		}
		b.WriteString("\n")
	}

	b.WriteString("\nJudge findings:\n")
	for _, v := range verdicts {
		for _, f := range v.Findings {
			fmt.Fprintf(&b, "- %s: %s\n", v.Judge, f)
		}
	}

	b.WriteString("\nRepair the change in the same workspace: address every finding above, " +
		"keep the original ticket satisfied, and do not revert unrelated work.\n")
	return b.String()
}
