package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/workspace"
)

// slowRunner simulates an agent run that takes a while, so the activity has
// time to heartbeat around it.
type slowRunner struct {
	delay time.Duration
}

func (r *slowRunner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	select {
	case <-time.After(r.delay):
		return ports.RunResult{CostUSD: 0.5, Output: "done"}, nil
	case <-ctx.Done():
		return ports.RunResult{}, ctx.Err()
	}
}

func TestActivities_RunAgent_HeartbeatsWhileAgentRuns(t *testing.T) {
	var beats atomic.Int32
	acts := &Activities{
		Agent:             &slowRunner{delay: 300 * time.Millisecond},
		HeartbeatInterval: 50 * time.Millisecond,
		heartbeat:         func(context.Context) { beats.Add(1) },
	}

	got, err := acts.RunAgent(context.Background(), RunAgentInput{
		Workspace: Workspace{Path: t.TempDir()},
		Prompt:    "implement the ticket",
	})

	require.NoError(t, err)
	require.InDelta(t, 0.5, got.CostUSD, 1e-9)
	require.GreaterOrEqual(t, beats.Load(), int32(3),
		"RunAgent must heartbeat repeatedly while the agent runs (crash-resume contract)")

	final := beats.Load()
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, final, beats.Load(), "heartbeating must stop once the agent returns")
}

func TestActivities_RunAgent_ReturnsAgentCost(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()

	acts := &Activities{
		Agent:             &slowRunner{delay: time.Millisecond},
		HeartbeatInterval: time.Second,
	}
	env.RegisterActivity(acts.RunAgent)

	val, err := env.ExecuteActivity(acts.RunAgent, RunAgentInput{Workspace: Workspace{Path: t.TempDir()}, Prompt: "noop"})
	require.NoError(t, err)

	var got AgentResult
	require.NoError(t, val.Get(&got))
	require.InDelta(t, 0.5, got.CostUSD, 1e-9)
}

// billedRunner simulates a harness that produced a parseable result
// envelope before failing: the run is billed, and RunAgent must map it to a
// non-retryable AgentRunBilled application error carrying the partial
// result (ADR-0006 §8).
type billedRunner struct{ result ports.RunResult }

func (r billedRunner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	return ports.RunResult{}, &ports.RunError{
		Result: r.result,
		Err:    errAgentBilledFixture,
	}
}

var errAgentBilledFixture = fmt.Errorf("agent reported is_error=true")

// modeledRunner reports a fixed resolved model on success, the way the
// adapter's model resolution (Task 4) does.
type modeledRunner struct{ model string }

func (r modeledRunner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	return ports.RunResult{CostUSD: 0.2, Output: "ok", Model: r.model}, nil
}

func TestActivities_RunAgent_PortRunError_MapsToNonRetryableAgentRunBilled(t *testing.T) {
	acts := &Activities{
		Agent: billedRunner{result: ports.RunResult{
			CostUSD: 0.42, Model: "claude-3-5-haiku", Usage: ports.TokenUsage{InputTokens: 3},
		}},
		HeartbeatInterval: time.Hour,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{Workspace: Workspace{Path: t.TempDir()}, Prompt: "p"})

	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, "AgentRunBilled", appErr.Type())
	require.True(t, appErr.NonRetryable())

	var details AgentResult
	require.NoError(t, appErr.Details(&details))
	require.InDelta(t, 0.42, details.CostUSD, 1e-9)
	require.Equal(t, "claude-3-5-haiku", details.Model)
	require.Equal(t, int64(3), details.Usage.InputTokens)
}

func TestActivities_RunAgent_Success_RecordsModelOnSpan(t *testing.T) {
	tel := newRecordingTelemetry(t)
	acts := &Activities{
		Agent:             modeledRunner{model: "claude-3-5-sonnet"},
		HeartbeatInterval: time.Hour,
		Telemetry:         tel.inst,
	}

	got, err := acts.RunAgent(context.Background(), RunAgentInput{
		JobID: "job-90", Workspace: Workspace{Path: t.TempDir()}, Prompt: "p",
	})
	require.NoError(t, err)
	require.Equal(t, "claude-3-5-sonnet", got.Model)

	ended := tel.spans.Ended()
	require.Len(t, ended, 1)
	got2 := spanAttrs(t, ended[0])
	require.Equal(t, "claude-3-5-sonnet", got2["gen_ai.response.model"].AsString())
}

func TestActivities_RunAgent_UnknownModel_LogsWarningNoFailure(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()

	acts := &Activities{Agent: modeledRunner{model: ports.ModelUnknown}, HeartbeatInterval: time.Hour}
	env.RegisterActivity(acts.RunAgent)

	val, err := env.ExecuteActivity(acts.RunAgent, RunAgentInput{Workspace: Workspace{Path: t.TempDir()}, Prompt: "p"})
	require.NoError(t, err, "an unresolved model must warn, never fail the run")

	var got AgentResult
	require.NoError(t, val.Get(&got))
	require.Equal(t, ports.ModelUnknown, got.Model)
}

// fakeCheckout records its call and returns a configurable error, so
// CheckoutWorkspace tests never touch real git.
type fakeCheckout struct {
	gotRepo, gotSHA, gotPath string
	err                      error
}

func (f *fakeCheckout) Checkout(ctx context.Context, repo, sha, path string) error {
	f.gotRepo, f.gotSHA, f.gotPath = repo, sha, path
	return f.err
}

func TestActivities_CheckoutWorkspace_DelegatesToPort(t *testing.T) {
	root := t.TempDir()
	fc := &fakeCheckout{}
	acts := &Activities{Checkout: fc, WorkspaceRoot: root}

	got, err := acts.CheckoutWorkspace(context.Background(), CheckoutInput{
		Repo: "/abs/repo", SourceRef: "abc123", JobID: "piece-1",
	})

	require.NoError(t, err)
	wantPath := filepath.Join(root, "tollgate-artifact-piece-1")
	require.Equal(t, Workspace{Path: wantPath}, got)
	require.Equal(t, "/abs/repo", fc.gotRepo)
	require.Equal(t, "abc123", fc.gotSHA)
	require.Equal(t, wantPath, fc.gotPath,
		"CheckoutWorkspace builds the path; the git adapter never does")
}

func TestActivities_CheckoutWorkspace_PropagatesPortError(t *testing.T) {
	fc := &fakeCheckout{err: ports.ErrRefNotFound}
	acts := &Activities{Checkout: fc, WorkspaceRoot: t.TempDir()}

	_, err := acts.CheckoutWorkspace(context.Background(), CheckoutInput{Repo: "/abs/repo", SourceRef: "x", JobID: "piece-2"})

	require.ErrorIs(t, err, ports.ErrRefNotFound)
}

func TestActivities_ApplyOverlay_HeartbeatsDuringLongCopy(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "output"), 0o755))
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "b.txt"), []byte("b"), 0o644))

	var beats atomic.Int32
	acts := &Activities{heartbeat: func(context.Context) { beats.Add(1) }}

	err := acts.ApplyOverlay(context.Background(), OverlayInput{
		Workspace:        Workspace{Path: ws},
		DestinationRoots: []string{"output"},
		Overlays:         []workspace.Overlay{{Source: srcDir, Dest: "output/copied"}},
	})

	require.NoError(t, err)
	require.GreaterOrEqual(t, beats.Load(), int32(1), "ApplyOverlay must heartbeat via the same seam as RunAgent")
}

func TestWithHeartbeat_StopsCleanlyOnCompletion(t *testing.T) {
	var beats atomic.Int32
	acts := &Activities{
		HeartbeatInterval: 20 * time.Millisecond,
		heartbeat:         func(context.Context) { beats.Add(1) },
	}

	err := acts.withHeartbeat(context.Background(), func() error {
		time.Sleep(100 * time.Millisecond)
		return nil
	})

	require.NoError(t, err)
	require.GreaterOrEqual(t, beats.Load(), int32(2))

	final := beats.Load()
	time.Sleep(80 * time.Millisecond)
	require.Equal(t, final, beats.Load(), "heartbeating must stop once work completes")
}

// capturingRunner records the RunSpec it was called with, so tests can
// assert what actually crossed the port boundary.
type capturingRunner struct {
	got  ports.RunSpec
	next ports.RunResult
}

func (r *capturingRunner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	r.got = spec
	return r.next, nil
}

// TestActivities_RunAgent_PassesAgentConfigToPort closes a wiring gap: an
// ArtifactJobInput's AgentConfig (Task 10) must actually reach the adapter
// through ports.RunSpec, or the whole agent-config plumbing added in Tasks
// 2-4 would be unreachable dead code for real artifact jobs.
func TestActivities_RunAgent_PassesAgentConfigToPort(t *testing.T) {
	runner := &capturingRunner{next: ports.RunResult{CostUSD: 0.1}}
	acts := &Activities{Agent: runner, HeartbeatInterval: time.Hour}
	cfg := json.RawMessage(`{"model":"sonnet"}`)

	_, err := acts.RunAgent(context.Background(), RunAgentInput{
		Workspace: Workspace{Path: t.TempDir()}, Prompt: "p", AgentConfig: cfg,
	})

	require.NoError(t, err)
	require.JSONEq(t, string(cfg), string(runner.got.AgentConfig))
}

func TestActivities_Prepare_CreatesIsolatedWorkspacePerJob(t *testing.T) {
	root := t.TempDir()
	acts := &Activities{WorkspaceRoot: root}

	ws, err := acts.Prepare(context.Background(), JobInput{JobID: "job-42"})

	require.NoError(t, err)
	require.Contains(t, ws.Path, "job-42")
	info, statErr := os.Stat(ws.Path)
	require.NoError(t, statErr)
	require.True(t, info.IsDir())
}
