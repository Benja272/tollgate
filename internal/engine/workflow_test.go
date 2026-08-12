package engine

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
)

func passVerdict(rubricVersion string) gate.Verdict {
	return gate.Verdict{
		Judge:         "haiku",
		RubricVersion: rubricVersion,
		Scores:        map[string]int{"correctness": 5, "clarity": 4},
	}
}

func TestJobWorkflow_HappyPath_RunsPhasesInOrderAndShips(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var mu sync.Mutex
	var order []string
	record := func(phase string) func(mock.Arguments) {
		return func(mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, phase)
		}
	}

	rubric := gate.Rubric{Name: "test", Version: "sha256:abc", Axes: []gate.Axis{{Name: "correctness", Blocking: true, MinScore: 4}}}

	var recorded []ports.CostEntry

	var acts *Activities
	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).
		Run(record("prepare")).
		Return(Workspace{Path: "/tmp/job-1"}, nil)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).
		Run(record("run_agent")).
		Return(AgentResult{CostUSD: 1.23, Output: "did the thing"}, nil)
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "record_cost")
			recorded = append(recorded, args.Get(1).([]ports.CostEntry)...)
		}).
		Return(nil)
	env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).
		Run(record("load_rubric")).
		Return(rubric, nil)
	env.OnActivity(acts.JudgeOne, mock.Anything, mock.Anything).
		Run(record("judge")).
		Return(ports.Judgment{Verdict: passVerdict(rubric.Version), CostUSD: 0.1}, nil)
	env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).
		Run(record("gate")).
		Return(gate.Decision{Outcome: gate.OutcomePass, Policy: gate.PolicyFailClosedV1, RubricVersion: rubric.Version}, nil)
	env.OnActivity(acts.Ship, mock.Anything, mock.Anything).
		Run(record("ship")).
		Return(ShipResult{PRURL: "https://github.com/Benja272/tollgate/pull/1"}, nil)

	env.ExecuteWorkflow(JobWorkflow, JobInput{
		JobID: "job-1", Repo: "Benja272/tollgate", SourceRef: "issue-42",
		Prompt: "implement the ticket", JudgeModels: []string{"haiku", "sonnet"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result JobResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, StatusShipped, result.Status)
	require.InDelta(t, 1.43, result.CostUSD, 1e-9, "job cost must aggregate agent AND judge costs (1.23 + 2×0.10)")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"prepare", "run_agent", "record_cost", "load_rubric", "judge", "judge", "record_cost", "gate", "ship"}, order,
		"one judge activity per configured judge model; costs recorded after the agent and after the judges")

	require.Len(t, recorded, 3, "one ledger entry for the agent plus one per judge")
	require.Equal(t, "agent", recorded[0].Actor)
	require.InDelta(t, 1.23, recorded[0].USD, 1e-9)
	require.Equal(t, "judge:haiku", recorded[1].Actor)
	require.Equal(t, "judge:sonnet", recorded[2].Actor)
	for _, e := range recorded {
		require.Equal(t, "job-1", e.JobID)
	}
}

func TestJobWorkflow_GateFail_RejectsWithoutShipping(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	rubric := gate.Rubric{Name: "test", Version: "sha256:abc", Axes: []gate.Axis{{Name: "correctness", Blocking: true, MinScore: 4}}}

	var acts *Activities
	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/job-2"}, nil)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{CostUSD: 0.5}, nil)
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).Return(rubric, nil)
	env.OnActivity(acts.JudgeOne, mock.Anything, mock.Anything).Return(ports.Judgment{Verdict: passVerdict(rubric.Version), CostUSD: 0.1}, nil)
	env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).
		Return(gate.Decision{Outcome: gate.OutcomeFail, Policy: gate.PolicyFailClosedV1, RubricVersion: rubric.Version, FailedBlocking: []string{"correctness"}}, nil)

	env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-2", Repo: "Benja272/tollgate", SourceRef: "issue-43", Prompt: "p"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result JobResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, StatusRejected, result.Status)
	require.Empty(t, result.PRURL)
	require.InDelta(t, 0.7, result.CostUSD, 1e-9, "a rejected job still reports its full cost, judges included (0.5 + 2×0.10)")
	env.AssertNotCalled(t, "Ship", mock.Anything, mock.Anything)
}

func TestJobWorkflow_RunAgentKeepsFailing_StopsAfterBoundedAttempts(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var acts *Activities
	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/job-3"}, nil)

	var mu sync.Mutex
	attempts := 0
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { mu.Lock(); attempts++; mu.Unlock() }).
		Return(AgentResult{}, errors.New("agent crashed"))

	env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-3", Repo: "Benja272/tollgate", SourceRef: "issue-44", Prompt: "p"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, runAgentMaxAttempts, attempts)
	env.AssertNotCalled(t, "JudgeOne", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "Ship", mock.Anything, mock.Anything)
}

func TestJobWorkflow_ActivityFailure_PropagatesAndShortCircuits(t *testing.T) {
	cases := []struct {
		name      string
		failing   string
		notCalled []string
	}{
		{"prepare fails", "Prepare", []string{"RunAgent", "RecordCosts", "LoadRubric", "JudgeOne", "DecideGate", "Ship"}},
		{"run agent fails", "RunAgent", []string{"RecordCosts", "LoadRubric", "JudgeOne", "DecideGate", "Ship"}},
		{"record costs fails", "RecordCosts", []string{"LoadRubric", "JudgeOne", "DecideGate", "Ship"}},
		{"load rubric fails", "LoadRubric", []string{"JudgeOne", "DecideGate", "Ship"}},
		{"judge fails", "JudgeOne", []string{"DecideGate", "Ship"}},
		{"gate decision fails", "DecideGate", []string{"Ship"}},
		{"ship fails", "Ship", nil},
	}

	rubric := gate.Rubric{Name: "test", Version: "sha256:abc", Axes: []gate.Axis{{Name: "correctness", Blocking: true, MinScore: 4}}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()

			var acts *Activities
			boom := temporal.NewNonRetryableApplicationError("boom", "TestFailure", nil)

			phases := []struct {
				name     string
				register func(err error)
			}{
				{"Prepare", func(err error) {
					env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/job"}, err)
				}},
				{"RunAgent", func(err error) {
					env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, err)
				}},
				{"RecordCosts", func(err error) {
					env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(err)
				}},
				{"LoadRubric", func(err error) {
					env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).Return(rubric, err)
				}},
				{"JudgeOne", func(err error) {
					env.OnActivity(acts.JudgeOne, mock.Anything, mock.Anything).Return(ports.Judgment{Verdict: passVerdict(rubric.Version), CostUSD: 0.1}, err)
				}},
				{"DecideGate", func(err error) {
					env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).Return(gate.Decision{Outcome: gate.OutcomePass}, err)
				}},
				{"Ship", func(err error) {
					env.OnActivity(acts.Ship, mock.Anything, mock.Anything).Return(ShipResult{}, err)
				}},
			}
			for _, p := range phases {
				if p.name == tc.failing {
					p.register(boom)
					break
				}
				p.register(nil)
			}

			env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-err", Repo: "Benja272/tollgate", SourceRef: "issue-45", Prompt: "p"})

			require.True(t, env.IsWorkflowCompleted())
			require.Error(t, env.GetWorkflowError())
			for _, name := range tc.notCalled {
				env.AssertNotCalled(t, name, mock.Anything, mock.Anything)
			}
		})
	}
}
