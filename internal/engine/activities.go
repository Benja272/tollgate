package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/log"
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

// defaultRunDeadlineMargin is how long before its activity deadline RunAgent
// kills the agent. A StartToClose timeout is decided by the server, which
// then retries whatever the activity returns later; killing the agent first
// lets RunAgent report the run as unmetered and non-retryable while its
// attempt is still live. Rounding the margin down for short deadlines keeps
// most of the budget for the agent.
const defaultRunDeadlineMargin = 30 * time.Second

// Temporal application error types for failures retrying cannot fix. Each
// maps a port sentinel at the activity boundary: a plain Go error reaches
// Temporal as retryable, whatever its documentation says.
const (
	errTypeCheckoutConflict         = "CheckoutConflict"
	errTypeInvalidRepo              = "InvalidRepo"
	errTypeRefNotFound              = "RefNotFound"
	errTypeOverlayOutsideRoots      = "OverlayOutsideRoots"
	errTypeOverlayUnsupportedSource = "OverlayUnsupportedSource"
	errTypeInvalidAgentConfig       = "InvalidAgentConfig"
	// errTypeAgentRunUnmetered marks a run killed before it reported its
	// cost (ADR-0006 §8): it may have billed an unknown amount, so it is
	// never retried automatically.
	errTypeAgentRunUnmetered = "AgentRunUnmetered"
)

// nonRetryable is one sentinel-to-type mapping for asNonRetryable.
type nonRetryable struct {
	sentinel error
	errType  string
}

// asNonRetryable wraps err as a non-retryable application error when it
// matches one of kinds, keeping err as the cause; otherwise err is returned
// unchanged.
func asNonRetryable(err error, kinds ...nonRetryable) error {
	for _, k := range kinds {
		if errors.Is(err, k.sentinel) {
			return temporal.NewNonRetryableApplicationError(err.Error(), k.errType, err)
		}
	}
	return err
}

// defaultAgentName labels the agent span when the wiring names no agent. The
// engine only knows the port, never which harness is behind it (ADR-0002).
const defaultAgentName = "coding-agent"

// Activities holds every side-effecting step of the job pipeline. It is the
// engine's dependency-injection point: fields are ports, wired to concrete
// adapters at worker startup.
type Activities struct {
	Agent ports.AgentRunner

	// Checkout prepares an artifact job's workspace as a pinned git
	// worktree (ADR-0006 §2).
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

	// RunDeadlineMargin is how long before the activity deadline the agent
	// is killed; zero means defaultRunDeadlineMargin. Tests shorten it.
	RunDeadlineMargin time.Duration
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

// workspaceRoot resolves where per-job workspaces are created: WorkspaceRoot
// when set, else the OS temp directory. Shared by Prepare and
// CheckoutWorkspace so both job shapes place workspaces the same way.
func (a *Activities) workspaceRoot() string {
	if a.WorkspaceRoot != "" {
		return a.WorkspaceRoot
	}
	return os.TempDir()
}

// Prepare creates the isolated workspace a job runs in. First cut: a fresh
// directory per job under WorkspaceRoot; the git-worktree checkout of the
// target repo is a later cycle.
func (a *Activities) Prepare(ctx context.Context, in JobInput) (Workspace, error) {
	path := filepath.Join(a.workspaceRoot(), "tollgate-job-"+in.JobID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return Workspace{}, fmt.Errorf("prepare workspace: %w", err)
	}
	return Workspace{Path: path}, nil
}

// RunAgent delegates to the AgentRunner port while heartbeating so the
// server can detect a dead worker mid-run instead of waiting out the
// activity timeout. The run is the job's largest spend, so it is also where
// the `invoke_agent` span and the cost metric are emitted (DESIGN.md §4).
//
// Failures are classified for Temporal (ADR-0006 §8):
//   - *ports.RunError: the harness reported its cost before failing. It
//     becomes a non-retryable AgentRunBilled error carrying cost, usage and
//     model, so runAgentAndRecord records the row before the job fails.
//   - *ports.UnmeteredRunError: the run was killed before reporting a cost.
//     It may have billed, so it becomes a non-retryable AgentRunUnmetered
//     error, logged and counted, and nothing is recorded.
//   - ports.ErrInvalidAgentConfig: non-retryable InvalidAgentConfig.
//   - anything else (the harness never ran, or exited without an envelope)
//     stays retryable within the policy's attempt cap.
func (a *Activities) RunAgent(ctx context.Context, in RunAgentInput) (AgentResult, error) {
	// Attempt 1 is the original agent; later attempts are the fix loop's
	// fixer actor (ADR-0003) — the span must agree with the ledger on that.
	actor := "agent"
	if in.Attempt > 1 {
		actor = "fixer"
	}
	call := telemetry.Call{
		JobID:     in.JobID,
		Phase:     "run_agent",
		Actor:     actor,
		AgentName: a.agentName(),
	}
	ctx, rec := a.Telemetry.StartInvokeAgent(ctx, call)

	runCtx, cancel := a.agentRunContext(ctx)
	defer cancel()

	var res ports.RunResult
	err := a.withHeartbeat(ctx, func() error {
		var runErr error
		res, runErr = a.Agent.Run(runCtx, ports.RunSpec{
			WorkspacePath: in.Workspace.Path, Prompt: in.Prompt, AgentConfig: in.AgentConfig,
		})
		return runErr
	})

	var billedErr *ports.RunError
	if errors.As(err, &billedErr) {
		res = billedErr.Result
	}
	rec.End(ctx, telemetry.Result{Usage: res.Usage, CostUSD: res.CostUSD, Model: res.Model}, err)

	var unmeteredErr *ports.UnmeteredRunError
	switch {
	case err == nil:
	case billedErr != nil:
		// Output stays out of the details: it can be large, and failure
		// payloads are bounded by Temporal's size limit.
		details := AgentResult{CostUSD: res.CostUSD, Usage: res.Usage, Model: res.Model}
		return AgentResult{}, temporal.NewNonRetryableApplicationError(
			billedErr.Error(), errTypeAgentRunBilled, nil, details)
	case errors.As(err, &unmeteredErr):
		activityLogger(ctx).Error("unmetered agent run: killed before reporting its cost; not retried",
			"job_id", in.JobID, "attempt", in.Attempt, "error", err)
		a.Telemetry.RecordUnmeteredRun(ctx, call)
		return AgentResult{}, temporal.NewNonRetryableApplicationError(
			err.Error(), errTypeAgentRunUnmetered, err)
	default:
		return AgentResult{}, asNonRetryable(err, nonRetryable{ports.ErrInvalidAgentConfig, errTypeInvalidAgentConfig})
	}

	if res.Model == ports.ModelUnknown {
		// An empty label would break "every row records the model"
		// (ADR-0006 §7); the run itself never fails over this.
		activityLogger(ctx).Warn("agent run resolved to an unknown model",
			"job_id", in.JobID, "attempt", in.Attempt)
	}
	return AgentResult{CostUSD: res.CostUSD, Usage: res.Usage, Output: res.Output, Model: res.Model}, nil
}

// agentRunContext bounds the agent run to end a margin before the activity
// deadline (see defaultRunDeadlineMargin). With no deadline it only adds
// cancellation.
func (a *Activities) agentRunContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	margin := a.RunDeadlineMargin
	if margin <= 0 {
		margin = defaultRunDeadlineMargin
	}
	if remaining := time.Until(deadline); margin > remaining/2 {
		margin = remaining / 2
	}
	return context.WithDeadline(ctx, deadline.Add(-margin))
}

// ValidateAgentConfig checks an artifact job's agent config before any
// workspace is prepared, so a bad config never costs a checkout or an
// overlay. The format belongs to the adapter (ADR-0002): a runner that
// cannot validate without running is trusted here and checked by RunAgent.
// An artifact job must name a model (ADR-0006 §5, §7).
func (a *Activities) ValidateAgentConfig(ctx context.Context, cfg json.RawMessage) error {
	v, ok := a.Agent.(ports.AgentConfigValidator)
	if !ok {
		return nil
	}
	model, err := v.ValidateConfig(cfg)
	if err == nil && model == "" {
		err = fmt.Errorf("%w: an artifact job must name a model", ports.ErrInvalidAgentConfig)
	}
	if err != nil {
		return asNonRetryable(err, nonRetryable{ports.ErrInvalidAgentConfig, errTypeInvalidAgentConfig})
	}
	return nil
}

// CheckoutInput identifies the pinned checkout an artifact job needs.
// JobID, not a path: the git adapter never builds the workspace path — CheckoutWorkspace does, the same way Prepare does for
// JobWorkflow, so both job shapes place workspaces under WorkspaceRoot
// identically. JobID is validated against the safe path-segment regex by
// the workflow's validate() before this activity ever runs.
type CheckoutInput struct {
	Repo      string
	SourceRef string
	JobID     string
}

// CheckoutWorkspace delegates to the Checkout port: a pinned, detached git
// worktree at exactly SourceRef, with repository hooks disabled (ADR-0006
// §2). It is its own activity, distinct from ApplyOverlay and the agent
// run, so a checkout retry never re-runs — and re-bills — either. The
// port's sentinels are permanent and reach Temporal as non-retryable.
func (a *Activities) CheckoutWorkspace(ctx context.Context, in CheckoutInput) (Workspace, error) {
	path := filepath.Join(a.workspaceRoot(), "tollgate-artifact-"+in.JobID)
	if err := a.Checkout.Checkout(ctx, in.Repo, in.SourceRef, path); err != nil {
		return Workspace{}, asNonRetryable(err,
			nonRetryable{ports.ErrCheckoutConflict, errTypeCheckoutConflict},
			nonRetryable{ports.ErrInvalidRepo, errTypeInvalidRepo},
			nonRetryable{ports.ErrRefNotFound, errTypeRefNotFound},
		)
	}
	return Workspace{Path: path}, nil
}

// OverlayInput carries what ApplyOverlay needs to place prepared files onto
// a checked-out workspace, bounded to declared destination roots.
type OverlayInput struct {
	Workspace        Workspace
	DestinationRoots []string
	Overlays         []workspace.Overlay
}

// ApplyOverlay places every overlay onto the workspace (ADR-0006 §3, §4).
// It is its own activity, distinct from the agent run, so an overlay
// failure never invokes — and never bills — the agent
// (workspace-preparation#Overlay Failure Isolation). It heartbeats through
// the same seam as RunAgent, both via the background ticker in
// withHeartbeat and via workspace.Apply's per-file beat callback, since a
// large overlay can take a while. Boundary and source violations are
// permanent and reach Temporal as non-retryable.
func (a *Activities) ApplyOverlay(ctx context.Context, in OverlayInput) error {
	err := a.withHeartbeat(ctx, func() error {
		return workspace.Apply(ctx, in.Workspace.Path, in.DestinationRoots, in.Overlays, func() { a.beat(ctx) })
	})
	return asNonRetryable(err,
		nonRetryable{workspace.ErrOutsideRoots, errTypeOverlayOutsideRoots},
		nonRetryable{workspace.ErrUnsupportedSource, errTypeOverlayUnsupportedSource},
	)
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

// activityLogger is the activity's logger, or the process default when
// called outside an activity (direct calls in tests).
func activityLogger(ctx context.Context) log.Logger {
	if activity.IsActivity(ctx) {
		return activity.GetLogger(ctx)
	}
	return log.NewStructuredLogger(slog.Default())
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
