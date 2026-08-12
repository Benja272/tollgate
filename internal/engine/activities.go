package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
)

// defaultHeartbeatInterval must stay well under the RunAgent
// HeartbeatTimeout (5s) so a single delayed beat never reads as a dead
// worker.
const defaultHeartbeatInterval = 2 * time.Second

// Activities holds every side-effecting step of the job pipeline. It is the
// engine's dependency-injection point: fields are ports, wired to concrete
// adapters at worker startup.
type Activities struct {
	Agent ports.AgentRunner

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
// activity timeout.
func (a *Activities) RunAgent(ctx context.Context, in RunAgentInput) (AgentResult, error) {
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

	res, err := a.Agent.Run(ctx, ports.RunSpec{WorkspacePath: in.Workspace.Path, Prompt: in.Prompt})
	if err != nil {
		return AgentResult{}, err
	}
	return AgentResult{CostUSD: res.CostUSD, Usage: res.Usage, Output: res.Output}, nil
}

// LoadRubric reads and content-addresses the rubric file. It is an
// activity because file I/O belongs outside workflow code, and journaling
// the loaded rubric pins the exact version every later phase uses.
func (a *Activities) LoadRubric(ctx context.Context, path string) (gate.Rubric, error) {
	return gate.LoadRubric(path)
}

// JudgeInput is one judgment request: which judge model, over what change.
type JudgeInput struct {
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
	return j.Judge(ctx, ports.JudgeRequest{Diff: in.Change, Ticket: in.Ticket, Rubric: in.Rubric})
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

func (a *Activities) heartbeatInterval() time.Duration {
	if a.HeartbeatInterval > 0 {
		return a.HeartbeatInterval
	}
	return defaultHeartbeatInterval
}
