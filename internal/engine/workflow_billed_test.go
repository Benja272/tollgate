package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/Benja272/tollgate/internal/ports"
)

// runAgentAndRecordTestWorkflow drives runAgentAndRecord the same way
// JobWorkflow's single call site does, so its RED tests do not need a full
// JobWorkflow run to exercise it in isolation.
func runAgentAndRecordTestWorkflow(ctx workflow.Context, in RunAgentInput) (AgentResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		// Isolate runAgentAndRecord from JobWorkflow's own retry budget:
		// these tests assert on a single RunAgent attempt.
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	return runAgentAndRecord(ctx, ctx, in, "agent", "piece-1")
}

func TestRunAgentAndRecord_Success_RecordsRowAndReturnsResult(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var acts *Activities
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).
		Return(AgentResult{CostUSD: 2.5, Output: "ok", Model: "claude-3-5-sonnet"}, nil)

	var recorded []ports.CostEntry
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			recorded = append(recorded, args.Get(1).([]ports.CostEntry)...)
		}).
		Return(nil)

	env.ExecuteWorkflow(runAgentAndRecordTestWorkflow, RunAgentInput{JobID: "job-1", Attempt: 1})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result AgentResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, AgentResult{CostUSD: 2.5, Output: "ok", Model: "claude-3-5-sonnet"}, result)

	require.Len(t, recorded, 1)
	require.Equal(t, "job-1", recorded[0].JobID)
	require.Equal(t, "run_agent", recorded[0].Phase)
	require.Equal(t, "agent", recorded[0].Actor)
	require.Equal(t, "claude-3-5-sonnet", recorded[0].Model)
	require.InDelta(t, 2.5, recorded[0].USD, 1e-9)
}

func TestRunAgentAndRecord_AgentRunBilled_RecordsRowThenReturnsError(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	billed := AgentResult{CostUSD: 0.75, Model: "claude-3-5-haiku", Usage: ports.TokenUsage{InputTokens: 5}}
	boom := temporal.NewNonRetryableApplicationError("agent reported is_error=true", "AgentRunBilled", nil, billed)

	var acts *Activities
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, boom)

	var recorded []ports.CostEntry
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			recorded = append(recorded, args.Get(1).([]ports.CostEntry)...)
		}).
		Return(nil)

	env.ExecuteWorkflow(runAgentAndRecordTestWorkflow, RunAgentInput{JobID: "job-2", Attempt: 1})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	require.Len(t, recorded, 1, "the billed row must be recorded before the function returns its error")
	require.Equal(t, "job-2", recorded[0].JobID)
	require.Equal(t, "claude-3-5-haiku", recorded[0].Model)
	require.InDelta(t, 0.75, recorded[0].USD, 1e-9)
	require.Equal(t, int64(5), recorded[0].Usage.InputTokens)

	var appErr *temporal.ApplicationError
	require.ErrorAs(t, env.GetWorkflowError(), &appErr)
	require.Equal(t, "AgentRunBilled", appErr.Type())
	require.True(t, appErr.NonRetryable())
}

func TestRunAgentAndRecord_UnparseableFailure_RecordsNothingStaysRetryable(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	// A crash/kill with no parseable envelope: a plain error, never
	// wrapped in an AgentRunBilled application error.
	crash := errors.New("agent process crashed")

	var acts *Activities
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, crash)
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(runAgentAndRecordTestWorkflow, RunAgentInput{JobID: "job-3", Attempt: 1})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertNotCalled(t, "RecordCosts", mock.Anything, mock.Anything)
}
