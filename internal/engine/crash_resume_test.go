package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// crashActivities mirrors the Activities method set so the workflow resolves
// them by name, but is instrumented for the crash-resume invariant: RunAgent
// attempt 1 blocks forever without heartbeating, simulating a worker that
// died mid-agent-run.
type crashActivities struct {
	prepareRuns  *atomic.Int32
	runAgentRuns *atomic.Int32
	release      chan struct{}
}

func (a *crashActivities) Prepare(ctx context.Context, in JobInput) (Workspace, error) {
	a.prepareRuns.Add(1)
	return Workspace{Path: "/tmp/crash-resume"}, nil
}

func (a *crashActivities) RunAgent(ctx context.Context, ws Workspace) (AgentResult, error) {
	a.runAgentRuns.Add(1)
	if activity.GetInfo(ctx).Attempt == 1 {
		<-a.release
		return AgentResult{}, ctx.Err()
	}
	return AgentResult{CostUSD: 2.5}, nil
}

func (a *crashActivities) Judge(ctx context.Context, ws Workspace) (JudgeReport, error) {
	return JudgeReport{}, nil
}

func (a *crashActivities) DecideGate(ctx context.Context, report JudgeReport) (GateDecision, error) {
	return GateDecision{Outcome: GatePass}, nil
}

func (a *crashActivities) Ship(ctx context.Context, ws Workspace) (ShipResult, error) {
	return ShipResult{PRURL: "https://github.com/Benja272/tollgate/pull/0"}, nil
}

func TestJobWorkflow_WorkerDiesMidAgentRun_ResumesAndShips(t *testing.T) {
	c, err := client.Dial(client.Options{})
	if err != nil {
		t.Skipf("temporal dev server not reachable, skipping integration test: %v", err)
	}
	defer c.Close()

	taskQueue := fmt.Sprintf("tollgate-crashtest-%d", time.Now().UnixNano())
	acts := &crashActivities{
		prepareRuns:  &atomic.Int32{},
		runAgentRuns: &atomic.Int32{},
		release:      make(chan struct{}),
	}
	defer close(acts.release)

	newWorker := func() worker.Worker {
		w := worker.New(c, taskQueue, worker.Options{WorkerStopTimeout: time.Second})
		w.RegisterWorkflow(JobWorkflow)
		w.RegisterActivity(acts)
		require.NoError(t, w.Start())
		return w
	}

	w1 := newWorker()

	workflowID := fmt.Sprintf("crash-resume-%d", time.Now().UnixNano())
	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: taskQueue,
	}, JobWorkflow, JobInput{JobID: "crash-1", Repo: "Benja272/tollgate", SourceRef: "issue-crash"})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = c.TerminateWorkflow(context.Background(), workflowID, "", "test cleanup")
	})

	require.Eventually(t, func() bool { return acts.runAgentRuns.Load() == 1 },
		15*time.Second, 50*time.Millisecond, "agent run never started on the first worker")

	w1.Stop()

	w2 := newWorker()
	defer w2.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result JobResult
	require.NoError(t, run.Get(ctx, &result), "job did not complete after worker crash")

	require.Equal(t, StatusShipped, result.Status)
	require.Equal(t, int32(1), acts.prepareRuns.Load(),
		"completed activities must not re-execute on resume (journal replay)")
	require.Equal(t, int32(2), acts.runAgentRuns.Load(),
		"agent run must retry exactly once after the crashed attempt")
}
