package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/workspace"
)

func validArtifactInput() ArtifactJobInput {
	return ArtifactJobInput{
		JobID:            "piece-42-plan",
		PieceID:          "piece-42",
		Repo:             "/abs/repo",
		SourceRef:        strings.Repeat("a", 40),
		Prompt:           "draft the plan",
		AgentConfig:      json.RawMessage(`{"model":"sonnet"}`),
		DestinationRoots: []string{"output"},
		Overlays:         []workspace.Overlay{{Source: "/abs/prepared/plan.md", Dest: "output/plan.md"}},
	}
}

func requireInvalidInput(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, "InvalidInput", appErr.Type())
	require.True(t, appErr.NonRetryable())
}

func TestArtifactJobWorkflow_Order_ChecksOutOverlaysRunsAgentRecordsCost(t *testing.T) {
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

	var acts *Activities
	env.OnActivity(acts.CheckoutWorkspace, mock.Anything, mock.Anything).
		Run(record("checkout")).
		Return(Workspace{Path: "/tmp/artifact-1"}, nil)
	env.OnActivity(acts.ApplyOverlay, mock.Anything, mock.Anything).
		Run(record("overlay")).
		Return(nil)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).
		Run(record("run_agent")).
		Return(AgentResult{CostUSD: 3.5, Output: "planned", Model: "claude-3-5-sonnet"}, nil)

	var recorded []ports.CostEntry
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "record_cost")
			recorded = append(recorded, args.Get(1).([]ports.CostEntry)...)
		}).
		Return(nil)

	env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result ArtifactJobResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "/tmp/artifact-1", result.Workspace)
	require.Equal(t, "claude-3-5-sonnet", result.Model)
	require.InDelta(t, 3.5, result.CostUSD, 1e-9)
	require.Equal(t, "planned", result.Output)

	require.Equal(t, []string{"checkout", "overlay", "run_agent", "record_cost"}, order)
	require.Len(t, recorded, 1)
	require.Equal(t, "piece-42", recorded[0].PieceID)

	env.AssertNotCalled(t, "Ship", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "JudgeOne", mock.Anything, mock.Anything)
}

func TestArtifactJobWorkflow_InvalidInput_TableDriven(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *ArtifactJobInput)
	}{
		{"branch ref", func(in *ArtifactJobInput) { in.SourceRef = "main" }},
		{"39-char sha", func(in *ArtifactJobInput) { in.SourceRef = strings.Repeat("a", 39) }},
		{"41-char sha", func(in *ArtifactJobInput) { in.SourceRef = strings.Repeat("a", 41) }},
		{"non-hex char", func(in *ArtifactJobInput) { in.SourceRef = "g" + strings.Repeat("a", 39) }},
		{"surrounding whitespace", func(in *ArtifactJobInput) { in.SourceRef = " " + strings.Repeat("a", 40) }},
		{"missing root", func(in *ArtifactJobInput) { in.DestinationRoots = nil }},
		{"root of dot", func(in *ArtifactJobInput) { in.DestinationRoots = []string{"."} }},
		{"root with .git segment", func(in *ArtifactJobInput) { in.DestinationRoots = []string{"output/.git"} }},
		{"escaping destination", func(in *ArtifactJobInput) {
			in.Overlays = []workspace.Overlay{{Source: "/abs/x", Dest: "output/../../etc/passwd"}}
		}},
		{"unsafe job_id", func(in *ArtifactJobInput) { in.JobID = "../etc" }},
		// nil, not json.RawMessage(""): an empty-but-non-nil RawMessage is
		// not valid JSON to begin with (its own MarshalJSON produces zero
		// bytes) and could never survive a real client's workflow-start
		// round trip. Omitting the field client-side is exactly nil here.
		{"empty agent_config", func(in *ArtifactJobInput) { in.AgentConfig = nil }},
		{"null agent_config", func(in *ArtifactJobInput) { in.AgentConfig = json.RawMessage(`null`) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()

			in := validArtifactInput()
			tc.mutate(&in)

			env.ExecuteWorkflow(ArtifactJobWorkflow, in)

			require.True(t, env.IsWorkflowCompleted())
			requireInvalidInput(t, env.GetWorkflowError())

			env.AssertNotCalled(t, "CheckoutWorkspace", mock.Anything, mock.Anything)
			env.AssertNotCalled(t, "ApplyOverlay", mock.Anything, mock.Anything)
			env.AssertNotCalled(t, "RunAgent", mock.Anything, mock.Anything)
			env.AssertNotCalled(t, "RecordCosts", mock.Anything, mock.Anything)
		})
	}
}

func TestArtifactJobWorkflow_UppercaseSHA_ReachesCheckoutLowercased(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	in := validArtifactInput()
	in.SourceRef = strings.ToUpper(in.SourceRef)

	var acts *Activities
	var gotSourceRef string
	env.OnActivity(acts.CheckoutWorkspace, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			gotSourceRef = args.Get(1).(CheckoutInput).SourceRef
		}).
		Return(Workspace{Path: "/tmp/artifact-2"}, nil)
	env.OnActivity(acts.ApplyOverlay, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{CostUSD: 1}, nil)
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(ArtifactJobWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, strings.ToLower(in.SourceRef), gotSourceRef)
}

func TestArtifactJobWorkflow_AgentRunBilled_RecordsRowThenFails(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var acts *Activities
	env.OnActivity(acts.CheckoutWorkspace, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/artifact-3"}, nil)
	env.OnActivity(acts.ApplyOverlay, mock.Anything, mock.Anything).Return(nil)

	billed := AgentResult{CostUSD: 0.9, Model: "claude-3-5-haiku"}
	boom := temporal.NewNonRetryableApplicationError("agent reported is_error=true", "AgentRunBilled", nil, billed)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, boom)

	var recorded []ports.CostEntry
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			recorded = append(recorded, args.Get(1).([]ports.CostEntry)...)
		}).
		Return(nil)

	env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Len(t, recorded, 1)
	require.Equal(t, "piece-42", recorded[0].PieceID)
	require.Equal(t, "claude-3-5-haiku", recorded[0].Model)
	require.InDelta(t, 0.9, recorded[0].USD, 1e-9)
}

func TestArtifactJobWorkflow_OverlayFailure_AgentNeverInvokedNoRunAgentRow(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var acts *Activities
	env.OnActivity(acts.CheckoutWorkspace, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/artifact-4"}, nil)
	env.OnActivity(acts.ApplyOverlay, mock.Anything, mock.Anything).
		Return(errors.New("overlay: symlink rejected"))

	env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertNotCalled(t, "RunAgent", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RecordCosts", mock.Anything, mock.Anything)
}
