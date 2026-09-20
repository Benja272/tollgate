package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"
)

// TestJobWorkflow_ReplaysShipHistory and TestJobWorkflow_ReplaysRejectHistory
// are the regression gate [Spec: artifact-job#Coexistence with the
// PR-Shaped JobWorkflow] promises: the two histories captured at the base
// commit (Task 1, before any production line of this change existed) must
// still replay cleanly against the FINAL JobWorkflow code. A replay failure
// here means Task 5's runAgentAndRecord extraction (or any later change)
// introduced non-determinism — wrong signal ordering, a renamed activity, or
// an added/removed step — and must be fixed in workflow.go, not worked
// around in this test.
func TestJobWorkflow_ReplaysShipHistory(t *testing.T) {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(JobWorkflow)

	err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/jobworkflow_ship.history.json")
	require.NoError(t, err, "the ship history must replay unchanged against the final JobWorkflow")
}

func TestJobWorkflow_ReplaysRejectHistory(t *testing.T) {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(JobWorkflow)

	err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/jobworkflow_reject.history.json")
	require.NoError(t, err, "the reject history must replay unchanged against the final JobWorkflow")
}

// TestArtifactJobWorkflow_ReplaysCapturedHistories is the determinism gate
// for ArtifactJobWorkflow, captured at the end of the post-archive review.
// No worker ran this workflow before then, which is why adding its first
// activity (ValidateAgentConfig) needed no workflow.GetVersion; from here
// on, any change that fails this replay does (ADR-0006 §10).
func TestArtifactJobWorkflow_ReplaysCapturedHistories(t *testing.T) {
	for _, name := range []string{"success", "billed"} {
		t.Run(name, func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(ArtifactJobWorkflow)

			err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/artifactjobworkflow_"+name+".history.json")
			require.NoError(t, err)
		})
	}
}
