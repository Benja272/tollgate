package engine

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

func TestJobWorkflow_HappyPath_RunsPhasesInOrderAndShips(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var order []string
	record := func(phase string) func(mock.Arguments) {
		return func(mock.Arguments) { order = append(order, phase) }
	}

	var acts *Activities
	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).
		Run(record("prepare")).
		Return(Workspace{Path: "/tmp/job-1"}, nil)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).
		Run(record("run_agent")).
		Return(AgentResult{CostUSD: 1.23}, nil)
	env.OnActivity(acts.Judge, mock.Anything, mock.Anything).
		Run(record("judge")).
		Return(JudgeReport{}, nil)
	env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).
		Run(record("gate")).
		Return(GateDecision{Outcome: GatePass}, nil)
	env.OnActivity(acts.Ship, mock.Anything, mock.Anything).
		Run(record("ship")).
		Return(ShipResult{PRURL: "https://github.com/Benja272/tollgate/pull/1"}, nil)

	env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-1", Repo: "Benja272/tollgate", SourceRef: "issue-42"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result JobResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, StatusShipped, result.Status)
	require.Equal(t, []string{"prepare", "run_agent", "judge", "gate", "ship"}, order)
}

func TestJobWorkflow_GateFail_RejectsWithoutShipping(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var acts *Activities
	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/job-2"}, nil)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{CostUSD: 0.5}, nil)
	env.OnActivity(acts.Judge, mock.Anything, mock.Anything).Return(JudgeReport{}, nil)
	env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).Return(GateDecision{Outcome: GateFail}, nil)

	env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-2", Repo: "Benja272/tollgate", SourceRef: "issue-43"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result JobResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, StatusRejected, result.Status)
	require.Empty(t, result.PRURL)
	env.AssertNotCalled(t, "Ship", mock.Anything, mock.Anything)
}

func TestJobWorkflow_RunAgentKeepsFailing_StopsAfterBoundedAttempts(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var acts *Activities
	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/job-3"}, nil)

	attempts := 0
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { attempts++ }).
		Return(AgentResult{}, errors.New("agent crashed"))

	env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-3", Repo: "Benja272/tollgate", SourceRef: "issue-44"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Equal(t, runAgentMaxAttempts, attempts)
	env.AssertNotCalled(t, "Judge", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "Ship", mock.Anything, mock.Anything)
}
