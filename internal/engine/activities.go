package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/telemetry"
	"github.com/Benja272/tollgate/internal/workspace"
)

// defaultHeartbeatInterval must stay well under the RunAgent
// HeartbeatTimeout (5s) so a single delayed beat never reads as a dead
// worker.
const defaultHeartbeatInterval = 2 * time.Second

// defaultAgentName labels the agent span when the wiring names no agent. The
// engine only knows the port, never which harness is behind it (ADR-0002).
const defaultAgentName = "coding-agent"

// Activities holds every side-effecting step of the job pipeline. It is the
// engine's dependency-injection point: fields are ports, wired to concrete
// adapters at worker startup.
type Activities struct {
	Agent ports.AgentRunner

	// Checkout prepares an artifact job's workspace as a pinned git
	// worktree (ADR-0006 D5, D6).
	Checkout ports.Checkout

	// Judges maps a judge model name to its implementation; JudgeModels in
	// JobInput select from here.
	Judges map[string]ports.Judge

	// Ledger persists cost entries (ADR-0004).
	Ledger ports.LedgerStore

	// WorkspaceRoot is where per-job workspaces are created; zero means the
	// OS temp directory.
	WorkspaceRoot string

	// HeartbeatInterval overrides the agent-run heartbeat cadence; zero
	// means defaultHeartbeatInterval. Tests shorten it.
	HeartbeatInterval time.Duration

	// AgentName names the wrapped harness in agent spans (gen_ai.agent.name);
	// zero means defaultAgentName.
	AgentName string

	// Telemetry records spans and metrics for the paid calls. Nil is a
	// working no-op: observability never gates job execution.
	Telemetry *telemetry.Instruments

	// heartbeat is the beat sink, injectable by tests; nil means
	// activity.RecordHeartbeat (the SDK test environment batches heartbeats,
	// so counting real ones is not observable there).
	heartbeat func(ctx context.Context)
}

func (a *Activities) beat(ctx context.Context) {
	if a.heartbeat != nil {
		a.heartbeat(ctx)
		return
	}
	activity.RecordHeartbeat(ctx)
}

// withHeartbeat runs work while heartbeating on a ticker in the background,
// stopping cleanly the moment work returns. Extracted from RunAgent's
// original inline loop so every long-running activity (RunAgent,
// ApplyOverlay) heartbeats identically through the same seam.
func (a *Activities) withHeartbeat(ctx context.Context, work func() error) error {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(a.heartbeatInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.beat(ctx)
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	return work()
}

// Prepare creates the isolated workspace a job runs in. First cut: a fresh
// directory per job under WorkspaceRoot; the git-worktree checkout of the
// target repo is a later cycle.
func (a *Activities) Prepare(ctx context.Context, in JobInput) (Workspace, error) {
	root := a.WorkspaceRoot
	if root == "" {
		root = os.TempDir()
	}
	path := filepath.Join(root, "tollgate-job-"+in.JobID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return Workspace{}, fmt.Errorf("prepare workspace: %w", err)
	}
	return Workspace{Path: path}, nil
}

// RunAgent delegates to the AgentRunner port while heartbeating so the
// server can detect a dead worker mid-run instead of waiting out the
// activity timeout. The run is the job's largest spend, so it is also where
// the `invoke_agent` span and the cost metric are emitted (DESIGN.md §4).
func (a *Activities) RunAgent(ctx context.Context, in RunAgentInput) (AgentResult, error) {
	// Attempt 1 is the original agent; later attempts are the fix loop's
	// fixer actor (ADR-0003) — the span must agree with the ledger on that.
	actor := "agent"
	if in.Attempt > 1 {
		actor = "fixer"
	}
	ctx, rec := a.Telemetry.StartInvokeAgent(ctx, telemetry.Call{
		JobID:     in.JobID,
		Phase:     "run_agent",
		Actor:     actor,
		AgentName: a.agentName(),
	})

	var res ports.RunResult
	err := a.withHeartbeat(ctx, func() error {
		var runErr error
		res, runErr = a.Agent.Run(ctx, ports.RunSpec{WorkspacePath: in.Workspace.Path, Prompt: in.Prompt})
		return runErr
	})
	rec.End(ctx, telemetry.Result{Usage: res.Usage, CostUSD: res.CostUSD, Model: res.Model}, err)
	if err != nil {
		// A *ports.RunError means the harness produced a parseable result
		// envelope before failing: the run was billed. Map it to a
		// non-retryable AgentRunBilled error carrying the partial result, so
		// runAgentAndRecord (workflow.go) can record the row before the job
		// fails (ADR-0006 D14). Any other error is a crash or a kill — no
		// envelope, nothing billed — and stays a plain, retryable error.
		var runErr *ports.RunError
		if errors.As(err, &runErr) {
			details := AgentResult{
				CostUSD: runErr.Result.CostUSD,
				Usage:   runErr.Result.Usage,
				Output:  runErr.Result.Output,
				Model:   runErr.Result.Model,
			}
			return AgentResult{}, temporal.NewNonRetryableApplicationError(
				runErr.Error(), errTypeAgentRunBilled, nil, details)
		}
		return AgentResult{}, err
	}
	if res.Model == ports.ModelUnknown {
		// An empty label would break "every row records the model"
		// (ADR-0006 D4); the run itself never fails over this.
		activity.GetLogger(ctx).Warn("agent run resolved to an unknown model",
			"job_id", in.JobID, "attempt", in.Attempt)
	}
	return AgentResult{CostUSD: res.CostUSD, Usage: res.Usage, Output: res.Output, Model: res.Model}, nil
}

// CheckoutInput identifies the pinned checkout an artifact job needs.
type CheckoutInput struct {
	Repo      string
	SourceRef string
	Path      string
}

// CheckoutWorkspace delegates to the Checkout port: a pinned, detached git
// worktree at exactly SourceRef, with repository hooks disabled (ADR-0006
// D5, D6). It is its own activity, distinct from ApplyOverlay and the agent
// run, so a checkout retry never re-runs — and re-bills — either.
func (a *Activities) CheckoutWorkspace(ctx context.Context, in CheckoutInput) (Workspace, error) {
	if err := a.Checkout.Checkout(ctx, in.Repo, in.SourceRef, in.Path); err != nil {
		return Workspace{}, err
	}
	return Workspace{Path: in.Path}, nil
}

// OverlayInput carries what ApplyOverlay needs to place prepared files onto
// a checked-out workspace, bounded to declared destination roots.
type OverlayInput struct {
	Workspace        Workspace
	DestinationRoots []string
	Overlays         []workspace.Overlay
}

// ApplyOverlay places every overlay onto the workspace (ADR-0006 D9-D11). It
// is its own activity, distinct from the agent run, so an overlay failure
// never invokes — and never bills — the agent (workspace-preparation#Overlay
// Failure Isolation). It heartbeats through the same seam as RunAgent, both
// via the background ticker in withHeartbeat and via workspace.Apply's
// per-overlay beat callback, since a large overlay can take a while.
func (a *Activities) ApplyOverlay(ctx context.Context, in OverlayInput) error {
	return a.withHeartbeat(ctx, func() error {
		return workspace.Apply(ctx, in.Workspace.Path, in.DestinationRoots, in.Overlays, func() { a.beat(ctx) })
	})
}

// LoadRubric reads and content-addresses the rubric file. It is an
// activity because file I/O belongs outside workflow code, and journaling
// the loaded rubric pins the exact version every later phase uses.
func (a *Activities) LoadRubric(ctx context.Context, path string) (gate.Rubric, error) {
	return gate.LoadRubric(path)
}

// JudgeInput is one judgment request: which judge model, over what change.
type JudgeInput struct {
	JobID  string
	Model  string
	Change string
	Ticket string
	Rubric gate.Rubric
}

// JudgeOne runs a single judge and reports its judgment — verdict plus
// cost. An unknown model is a configuration error, not a judgment —
// non-retryable, since retrying cannot fix wiring.
func (a *Activities) JudgeOne(ctx context.Context, in JudgeInput) (ports.Judgment, error) {
	j, ok := a.Judges[in.Model]
	if !ok {
		return ports.Judgment{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("no judge wired for model %q", in.Model), "JudgeConfig", nil)
	}

	// A judgment is a paid model call like the agent run, so it carries the
	// same span shape — actor and model included, since "which judge cost
	// what" is a first-class ledger question.
	call := telemetry.Call{
		JobID:     in.JobID,
		Phase:     "judge",
		Actor:     "judge:" + in.Model,
		AgentName: "judge:" + in.Model,
		Model:     in.Model,
	}
	ctx, rec := a.Telemetry.StartInvokeAgent(ctx, call)

	judgment, err := j.Judge(ctx, ports.JudgeRequest{Diff: in.Change, Ticket: in.Ticket, Rubric: in.Rubric})
	if err == nil {
		a.Telemetry.RecordJudgeScores(ctx, call, judgment.Verdict.RubricVersion, judgment.Verdict.Scores)
	}
	rec.End(ctx, telemetry.Result{Usage: judgment.Usage, CostUSD: judgment.CostUSD}, err)
	if err != nil {
		return ports.Judgment{}, err
	}
	return judgment, nil
}

// DecideInput carries the verdicts and the rubric they were judged against.
type DecideInput struct {
	Rubric   gate.Rubric
	Verdicts []gate.Verdict
}

// DecideGate applies the resolution policy. The heavy lifting is the pure
// gate.Decide; the activity exists so the decision lands in the journal.
func (a *Activities) DecideGate(ctx context.Context, in DecideInput) (gate.Decision, error) {
	return gate.Decide(in.Rubric, in.Verdicts)
}

// RecordCosts persists ledger entries. It is deliberately its own activity:
// a paid call and a cheap retryable write must never share one, or a
// failed write would re-run — and re-bill — the paid call.
func (a *Activities) RecordCosts(ctx context.Context, entries []ports.CostEntry) error {
	return a.Ledger.RecordCosts(ctx, entries)
}

// Ship is scaffolding: PR creation needs an idempotency design first
// (ADR-0001 consequence), so it currently ships nothing and reports no URL.
func (a *Activities) Ship(ctx context.Context, ws Workspace) (ShipResult, error) {
	return ShipResult{}, nil
}

func (a *Activities) agentName() string {
	if a.AgentName != "" {
		return a.AgentName
	}
	return defaultAgentName
}

func (a *Activities) heartbeatInterval() time.Duration {
	if a.HeartbeatInterval > 0 {
		return a.HeartbeatInterval
	}
	return defaultHeartbeatInterval
}
