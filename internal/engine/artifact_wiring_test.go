package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// TestArtifactJobWorkflow_RegistersAgainstRealActivityStruct is a pure
// wiring smoke test: worker.Register{Workflow,Activity} validate function
// and method signatures by reflection and panic synchronously on a
// mismatch — no live Temporal server is needed (client.NewLazyClient never
// dials). This is the cheapest possible guard against ArtifactJobWorkflow's
// workflow.ExecuteActivity(ctx, acts.CheckoutWorkspace, ...) (and the other
// activity references) drifting from the real *Activities method set.
func TestArtifactJobWorkflow_RegistersAgainstRealActivityStruct(t *testing.T) {
	c, err := client.NewLazyClient(client.Options{})
	require.NoError(t, err)
	defer c.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering ArtifactJobWorkflow/Activities panicked: %v", r)
		}
	}()

	w := worker.New(c, TaskQueue, worker.Options{})
	w.RegisterWorkflow(ArtifactJobWorkflow)
	w.RegisterActivity(&Activities{})
}
