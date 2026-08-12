package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"

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
	return AgentResult{CostUSD: res.CostUSD, Output: res.Output}, nil
}

// Judge is scaffolding: the rubric engine and N-judge fan-out are the gate
// milestone (ADR-0003); until then every attempt yields an empty report.
func (a *Activities) Judge(ctx context.Context, ws Workspace) (JudgeReport, error) {
	return JudgeReport{}, nil
}

// DecideGate is scaffolding for the same milestone: with no verdicts there
// is nothing to fail on, so the empty report passes. The real resolution
// policy (fail-closed over blocking axes) replaces this.
func (a *Activities) DecideGate(ctx context.Context, report JudgeReport) (GateDecision, error) {
	return GateDecision{Outcome: GatePass}, nil
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
