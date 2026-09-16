package engine

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/Benja272/tollgate/internal/ports"
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
// result (ADR-0006 D14).
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
