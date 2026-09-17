package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/Benja272/tollgate/internal/adapters/claudecode"
	"github.com/Benja272/tollgate/internal/adapters/gitcli"
	"github.com/Benja272/tollgate/internal/adapters/postgres"
	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/workspace"
)

// resetAfter resets a completed execution to the first workflow task that
// completed after the last completion of activityType, and waits for the
// new run to finish. The new run re-issues every command that task issued
// — the RecordCosts after a paid activity — while the paid activity itself
// stays in the copied history and is not re-run.
func resetAfter(t *testing.T, c client.Client, workflowID, runID, activityType string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scheduled := map[int64]string{}
	var sawCompletion bool
	var resetTo int64
	iter := c.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		ev, err := iter.Next()
		require.NoError(t, err)
		switch ev.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			scheduled[ev.GetEventId()] = ev.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName()
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			if scheduled[ev.GetActivityTaskCompletedEventAttributes().GetScheduledEventId()] == activityType {
				sawCompletion, resetTo = true, 0
			}
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED:
			if sawCompletion && resetTo == 0 {
				resetTo = ev.GetEventId()
			}
		}
	}
	require.NotZero(t, resetTo, "no workflow task completed after %s", activityType)

	resp, err := c.ResetWorkflowExecution(ctx, &workflowservice.ResetWorkflowExecutionRequest{
		Namespace:                 "default",
		WorkflowExecution:         &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: runID},
		Reason:                    "test: reset between the paid activity and its cost row",
		WorkflowTaskFinishEventId: resetTo,
		RequestId:                 fmt.Sprintf("reset-%d", time.Now().UnixNano()),
	})
	require.NoError(t, err)
	require.NotEqual(t, runID, resp.GetRunId())
	require.NoError(t, c.GetWorkflow(ctx, workflowID, resp.GetRunId()).Get(ctx, nil), "the reset run did not complete")
	return resp.GetRunId()
}

func countRows(t *testing.T, pool *pgxpool.Pool, jobID string) (rows int, usd float64) {
	t.Helper()
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(usd), 0)::float8 FROM cost_entries WHERE job_id = $1`, jobID).Scan(&rows, &usd))
	return rows, usd
}

// A workflow reset between RunAgent and RecordCosts used to write the same
// billed run again under the new run id: one paid run, two rows (review R1,
// reproduced). The row must be keyed by the run that PAID.
func TestArtifactJobWorkflow_E2E_ResetAfterRunAgent_NoDoubleCount(t *testing.T) {
	c := dialOrSkip(t)
	defer c.Close()
	pool := ledgerPoolOrSkip(t)
	ledger := postgres.NewLedger(pool)

	repo, sha := e2eGitRepo(t)
	overlaySrc := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(overlaySrc, []byte("the plan"), 0o644))
	claudeBin := e2eFakeClaude(t, `{"type":"result","is_error":false,"total_cost_usd":0.50,"result":"ok","modelUsage":{"claude-haiku-4-5":{"costUSD":0.50}}}`)

	acts := &Activities{
		Agent:             &claudecode.Runner{Bin: claudeBin},
		Checkout:          gitcli.Checkout{},
		Ledger:            ledger,
		WorkspaceRoot:     t.TempDir(),
		HeartbeatInterval: time.Hour,
	}
	taskQueue := fmt.Sprintf("tollgate-reset-artifact-%d", time.Now().UnixNano())
	w := e2eWorker(t, c, taskQueue, acts)
	defer w.Stop()

	stamp := time.Now().UnixNano()
	in := ArtifactJobInput{
		JobID:            fmt.Sprintf("e2e-reset-%d", stamp),
		PieceID:          fmt.Sprintf("piece-e2e-reset-%d", stamp),
		Repo:             repo,
		SourceRef:        sha,
		Prompt:           "render",
		AgentConfig:      json.RawMessage(`{"model":"haiku"}`),
		DestinationRoots: []string{"output"},
		Overlays:         []workspace.Overlay{{Source: overlaySrc, Dest: "output/plan.md"}},
	}
	workflowID := "artifact-" + in.JobID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: taskQueue}, ArtifactJobWorkflow, in)
	require.NoError(t, err)
	require.NoError(t, run.Get(ctx, nil))

	resetAfter(t, c, workflowID, run.GetRunID(), "RunAgent")

	rows, usd := countRows(t, pool, in.JobID)
	require.Equal(t, 1, rows, "one paid run must be one row, even after a reset")
	require.InDelta(t, 0.50, usd, 1e-9)
	spend, err := ledger.PerPieceSpend(context.Background(), in.PieceID)
	require.NoError(t, err)
	require.Len(t, spend, 1)
	require.InDelta(t, 0.50, spend[0].USD, 1e-9)
}

// passingJudge passes every axis of whatever rubric it is given.
type passingJudge struct{ cost float64 }

func (j passingJudge) Judge(_ context.Context, req ports.JudgeRequest) (ports.Judgment, error) {
	scores := map[string]int{}
	for _, ax := range req.Rubric.Axes {
		scores[ax.Name] = gate.ScaleMax
	}
	return ports.Judgment{
		Verdict: gate.Verdict{Judge: "fake", RubricVersion: req.Rubric.Version, Scores: scores},
		CostUSD: j.cost,
	}, nil
}

func TestJobWorkflow_E2E_ResetAfterRunAgentAndAfterJudge_NoDoubleCount(t *testing.T) {
	// Resetting after RunAgent re-runs the judge in the new run: that is a
	// second paid call and correctly gets its own row. The agent run was
	// paid once and must stay one row.
	cases := []struct {
		activityType string
		wantRows     int
		wantUSD      float64
	}{
		{"RunAgent", 3, 0.1 + 0.25 + 0.25},
		{"JudgeOne", 2, 0.1 + 0.25},
	}
	for _, tc := range cases {
		activityType := tc.activityType
		t.Run(activityType, func(t *testing.T) {
			c := dialOrSkip(t)
			defer c.Close()
			pool := ledgerPoolOrSkip(t)
			ledger := postgres.NewLedger(pool)

			acts := &Activities{
				Agent:             &validatingRunner{model: "claude-haiku-4-5"},
				Judges:            map[string]ports.Judge{"haiku": passingJudge{cost: 0.25}},
				Ledger:            ledger,
				WorkspaceRoot:     t.TempDir(),
				HeartbeatInterval: time.Hour,
			}
			taskQueue := fmt.Sprintf("tollgate-reset-job-%d", time.Now().UnixNano())
			w := worker.New(c, taskQueue, worker.Options{WorkerStopTimeout: time.Second})
			w.RegisterWorkflow(JobWorkflow)
			w.RegisterActivity(acts)
			require.NoError(t, w.Start())
			defer w.Stop()

			jobID := fmt.Sprintf("e2e-reset-job-%d", time.Now().UnixNano())
			workflowID := "job-" + jobID
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: taskQueue}, JobWorkflow, JobInput{
				JobID: jobID, Prompt: "p", JudgeModels: []string{"haiku"},
				RubricPath: filepath.Join("..", "..", "rubrics", "default.yaml"),
			})
			require.NoError(t, err)
			require.NoError(t, run.Get(ctx, nil))

			resetAfter(t, c, workflowID, run.GetRunID(), activityType)

			rows, usd := countRows(t, pool, jobID)
			require.Equal(t, tc.wantRows, rows, "each paid call is one row, even after a reset")
			require.InDelta(t, tc.wantUSD, usd, 1e-9)
		})
	}
}
