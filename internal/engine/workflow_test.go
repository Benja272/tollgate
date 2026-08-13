package engine

import (
	"context"
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
	require.Equal(t, []string{"prepare", "load_rubric", "run_agent", "record_cost", "judge", "judge", "record_cost", "gate", "ship"}, order,
		"the rubric is loaded before the agent is paid for; one judge activity per configured judge model; costs recorded after the agent and after the judges")

	require.Len(t, recorded, 3, "one ledger entry for the agent plus one per judge")
	require.Equal(t, "agent", recorded[0].Actor)
	require.InDelta(t, 1.23, recorded[0].USD, 1e-9)
	require.Equal(t, "judge:haiku", recorded[1].Actor)
	require.Equal(t, "judge:sonnet", recorded[2].Actor)
	for _, e := range recorded {
		require.Equal(t, "job-1", e.JobID)
	}
}

// scriptedRun wires one workflow run whose gate outcome is scripted per
// attempt, and captures what the agent was asked and what was ledgered.
type scriptedRun struct {
	mu          sync.Mutex
	agentInputs []RunAgentInput
	judgeInputs []JudgeInput
	recorded    []ports.CostEntry
}

func (r *scriptedRun) agentRuns() []RunAgentInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RunAgentInput(nil), r.agentInputs...)
}

func (r *scriptedRun) entries() []ports.CostEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ports.CostEntry(nil), r.recorded...)
}

// failVerdict scores the blocking axis below its minimum and carries the
// findings the fix loop must feed back to the agent.
func failVerdict(judge, rubricVersion string, findings ...string) gate.Verdict {
	return gate.Verdict{
		Judge:         judge,
		RubricVersion: rubricVersion,
		Scores:        map[string]int{"correctness": 2, "clarity": 4},
		Findings:      findings,
	}
}

// scriptGate registers one gate decision per attempt plus the surrounding
// activity mocks. agentCosts[i] is what attempt i+1's agent run costs, so a
// total that only counts one attempt is visibly wrong.
func scriptGate(env *testsuite.TestWorkflowEnvironment, rubric gate.Rubric, outcomes []gate.Outcome, agentCosts []float64) *scriptedRun {
	run := &scriptedRun{}
	var acts *Activities

	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/job-fix"}, nil)
	env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).Return(rubric, nil)

	for i := range outcomes {
		cost := 0.0
		if i < len(agentCosts) {
			cost = agentCosts[i]
		}
		env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				run.mu.Lock()
				defer run.mu.Unlock()
				run.agentInputs = append(run.agentInputs, args.Get(1).(RunAgentInput))
			}).
			Return(AgentResult{CostUSD: cost, Output: "diff"}, nil).Once()
	}

	env.OnActivity(acts.JudgeOne, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			run.mu.Lock()
			defer run.mu.Unlock()
			run.judgeInputs = append(run.judgeInputs, args.Get(1).(JudgeInput))
		}).
		Return(func(_ context.Context, in JudgeInput) (ports.Judgment, error) {
			return ports.Judgment{
				Verdict: failVerdict(in.Model, in.Rubric.Version, in.Model+" says: missing test for the edge case"),
				CostUSD: 0.1,
			}, nil
		})

	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			run.mu.Lock()
			defer run.mu.Unlock()
			run.recorded = append(run.recorded, args.Get(1).([]ports.CostEntry)...)
		}).
		Return(nil)

	for _, o := range outcomes {
		d := gate.Decision{Outcome: o, Policy: gate.PolicyFailClosedV1, RubricVersion: rubric.Version}
		if o == gate.OutcomeFail {
			d.FailedBlocking = []string{"correctness"}
		}
		env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).Return(d, nil).Once()
	}

	env.OnActivity(acts.Ship, mock.Anything, mock.Anything).
		Return(ShipResult{PRURL: "https://github.com/Benja272/tollgate/pull/7"}, nil)

	return run
}

func TestJobWorkflow_FixLoop(t *testing.T) {
	rubric := gate.Rubric{Name: "test", Version: "sha256:abc", Axes: []gate.Axis{
		{Name: "correctness", Blocking: true, MinScore: 4},
		{Name: "clarity", Blocking: false, MinScore: 3},
	}}

	pass, fail := gate.OutcomePass, gate.OutcomeFail

	cases := []struct {
		name           string
		maxFixAttempts int
		outcomes       []gate.Outcome
		agentCosts     []float64
		wantStatus     JobStatus
		wantAgentRuns  int
		wantCost       float64
	}{
		{
			name:          "pass on the first attempt never invokes the fixer",
			outcomes:      []gate.Outcome{pass},
			agentCosts:    []float64{1.0},
			wantStatus:    StatusShipped,
			wantAgentRuns: 1,
			wantCost:      1.0 + 2*0.1,
		},
		{
			name:          "fail then fix passes ships",
			outcomes:      []gate.Outcome{fail, pass},
			agentCosts:    []float64{1.0, 0.5},
			wantStatus:    StatusShipped,
			wantAgentRuns: 2,
			wantCost:      1.0 + 2*0.1 + 0.5 + 2*0.1,
		},
		{
			name:          "default budget exhausted rejects after two fix attempts",
			outcomes:      []gate.Outcome{fail, fail, fail},
			agentCosts:    []float64{1.0, 0.5, 0.25},
			wantStatus:    StatusRejected,
			wantAgentRuns: 1 + defaultMaxFixAttempts,
			wantCost:      1.0 + 0.5 + 0.25 + 3*2*0.1,
		},
		{
			name:           "MaxFixAttempts zero uses the default budget",
			maxFixAttempts: 0,
			outcomes:       []gate.Outcome{fail, fail, fail},
			agentCosts:     []float64{1.0, 0.5, 0.25},
			wantStatus:     StatusRejected,
			wantAgentRuns:  1 + defaultMaxFixAttempts,
			wantCost:       1.0 + 0.5 + 0.25 + 3*2*0.1,
		},
		{
			name:           "MaxFixAttempts one stops after a single repair",
			maxFixAttempts: 1,
			outcomes:       []gate.Outcome{fail, fail},
			agentCosts:     []float64{1.0, 0.5},
			wantStatus:     StatusRejected,
			wantAgentRuns:  2,
			wantCost:       1.0 + 0.5 + 2*2*0.1,
		},
		{
			name:           "negative MaxFixAttempts disables the fix loop",
			maxFixAttempts: -1,
			outcomes:       []gate.Outcome{fail},
			agentCosts:     []float64{1.0},
			wantStatus:     StatusRejected,
			wantAgentRuns:  1,
			wantCost:       1.0 + 2*0.1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			run := scriptGate(env, rubric, tc.outcomes, tc.agentCosts)

			env.ExecuteWorkflow(JobWorkflow, JobInput{
				JobID: "job-fix", Repo: "Benja272/tollgate", SourceRef: "issue-50",
				Prompt: "implement the ticket", JudgeModels: []string{"haiku", "sonnet"},
				MaxFixAttempts: tc.maxFixAttempts,
			})

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())

			var result JobResult
			require.NoError(t, env.GetWorkflowResult(&result))
			require.Equal(t, tc.wantStatus, result.Status)
			require.InDelta(t, tc.wantCost, result.CostUSD, 1e-9,
				"job cost must aggregate every attempt: agent, fixer runs and all judge rounds")

			agentRuns := run.agentRuns()
			require.Len(t, agentRuns, tc.wantAgentRuns, "one agent run per attempt, no more")

			// Attempt numbering is dense and 1-based across the whole job.
			entries := run.entries()
			for attempt := 1; attempt <= tc.wantAgentRuns; attempt++ {
				wantActor := "fixer"
				if attempt == 1 {
					wantActor = "agent"
				}
				require.Contains(t, entries, ports.CostEntry{
					JobID: "job-fix", Phase: "run_agent", Actor: wantActor,
					USD: tc.agentCosts[attempt-1], Attempt: int32(attempt),
				}, "ledger must carry an attempt-numbered run_agent entry for attempt %d", attempt)

				for _, model := range []string{"haiku", "sonnet"} {
					require.Contains(t, entries, ports.CostEntry{
						JobID: "job-fix", Phase: "judge", Actor: "judge:" + model,
						Model: model, USD: 0.1, Attempt: int32(attempt),
					}, "every re-judging round must be ledgered under its own attempt")
				}
			}
			require.Len(t, entries, tc.wantAgentRuns*3, "one agent entry plus one per judge, per attempt")

			if tc.wantStatus == StatusShipped {
				require.NotEmpty(t, result.PRURL)
			} else {
				require.Empty(t, result.PRURL)
				env.AssertNotCalled(t, "Ship", mock.Anything, mock.Anything)
			}
		})
	}
}

func TestJobWorkflow_FixAttempt_RepairPromptCarriesTicketAndFindings(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	rubric := gate.Rubric{Name: "test", Version: "sha256:abc", Axes: []gate.Axis{
		{Name: "correctness", Blocking: true, MinScore: 4},
	}}
	run := scriptGate(env, rubric, []gate.Outcome{gate.OutcomeFail, gate.OutcomePass}, []float64{1.0, 0.5})

	env.ExecuteWorkflow(JobWorkflow, JobInput{
		JobID: "job-repair", Repo: "Benja272/tollgate", SourceRef: "issue-51",
		Prompt: "implement the ticket", JudgeModels: []string{"haiku", "sonnet"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	runs := run.agentRuns()
	require.Len(t, runs, 2)

	require.Equal(t, int32(1), runs[0].Attempt)
	require.Equal(t, "implement the ticket", runs[0].Prompt, "the first run gets the ticket verbatim")

	fix := runs[1]
	require.Equal(t, int32(2), fix.Attempt)
	require.Equal(t, runs[0].Workspace, fix.Workspace, "a fix attempt repairs the same workspace")
	require.Contains(t, fix.Prompt, "implement the ticket", "the repair prompt must restate the original ticket")
	require.Contains(t, fix.Prompt, "correctness", "the repair prompt must name the failed blocking axis")
	require.Contains(t, fix.Prompt, "haiku says: missing test for the edge case")
	require.Contains(t, fix.Prompt, "sonnet says: missing test for the edge case")

	run.mu.Lock()
	defer run.mu.Unlock()
	require.Len(t, run.judgeInputs, 4, "every attempt is re-judged in full, by every judge")
	for _, ji := range run.judgeInputs {
		require.Equal(t, "implement the ticket", ji.Ticket,
			"judges always score against the original ticket, never the repair prompt")
		require.Equal(t, rubric.Version, ji.Rubric.Version,
			"re-judging must pin the same rubric version as the first attempt (ADR-0003)")
	}
}

func TestRepairPrompt_IsDeterministicAndFeedsBackBlockingFindings(t *testing.T) {
	rubric := gate.Rubric{Name: "test", Version: "sha256:abc", Axes: []gate.Axis{
		{Name: "correctness", Blocking: true, MinScore: 4},
		{Name: "safety", Blocking: true, MinScore: 4},
		{Name: "clarity", Blocking: false, MinScore: 3},
	}}
	verdicts := []gate.Verdict{
		{Judge: "haiku", RubricVersion: rubric.Version, Scores: map[string]int{"correctness": 2, "safety": 5, "clarity": 4}, Findings: []string{"no test for nil input"}},
		{Judge: "sonnet", RubricVersion: rubric.Version, Scores: map[string]int{"correctness": 5, "safety": 5, "clarity": 2}, Findings: []string{"naming is opaque"}},
	}
	decision := gate.Decision{
		Outcome: gate.OutcomeFail, Policy: gate.PolicyFailClosedV1, RubricVersion: rubric.Version,
		FailedBlocking: []string{"correctness"}, FailedAdvisory: []string{"clarity"},
	}

	got := repairPrompt("implement the ticket", rubric, decision, verdicts, 2, 2)

	require.Contains(t, got, "implement the ticket")
	require.Contains(t, got, rubric.Version, "the fixer must know which rubric version blocked it")
	require.Contains(t, got, "correctness")
	require.Contains(t, got, "min score 4")
	require.Contains(t, got, "haiku scored 2")
	require.Contains(t, got, "no test for nil input")
	require.Contains(t, got, "naming is opaque")
	require.NotContains(t, got, "safety", "only the axes that actually blocked are fed back")

	// Same inputs, same bytes: a workflow replay must rebuild the identical
	// prompt, so nothing here may iterate a Go map.
	for i := 0; i < 20; i++ {
		require.Equal(t, got, repairPrompt("implement the ticket", rubric, decision, verdicts, 2, 2))
	}
}

func TestJobWorkflow_RunAgentKeepsFailing_StopsAfterBoundedAttempts(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var acts *Activities
	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/job-3"}, nil)
	env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).
		Return(gate.Rubric{Name: "test", Version: "sha256:abc", Axes: []gate.Axis{{Name: "correctness", Blocking: true, MinScore: 4}}}, nil)

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
		{"prepare fails", "Prepare", []string{"LoadRubric", "RunAgent", "RecordCosts", "JudgeOne", "DecideGate", "Ship"}},
		{"load rubric fails", "LoadRubric", []string{"RunAgent", "RecordCosts", "JudgeOne", "DecideGate", "Ship"}},
		{"run agent fails", "RunAgent", []string{"RecordCosts", "JudgeOne", "DecideGate", "Ship"}},
		{"record costs fails", "RecordCosts", []string{"JudgeOne", "DecideGate", "Ship"}},
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
				{"LoadRubric", func(err error) {
					env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).Return(rubric, err)
				}},
				{"RunAgent", func(err error) {
					env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, err)
				}},
				{"RecordCosts", func(err error) {
					env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(err)
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
