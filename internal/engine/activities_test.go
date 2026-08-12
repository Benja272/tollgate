package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

	got, err := acts.RunAgent(context.Background(), Workspace{Path: t.TempDir()})

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

	val, err := env.ExecuteActivity(acts.RunAgent, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	var got AgentResult
	require.NoError(t, val.Get(&got))
	require.InDelta(t, 0.5, got.CostUSD, 1e-9)
}
