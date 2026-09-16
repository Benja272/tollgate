package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
)

// captureRubric is a minimal, fixed rubric used only to produce the two
// frozen-engine histories: one blocking axis, scale 1..5, min score 4.
var captureRubric = gate.Rubric{
	Name:    "capture",
	Version: "sha256:capture-fixture",
	Axes:    []gate.Axis{{Name: "correctness", Blocking: true, MinScore: 4}},
}

// captureActivities mirrors the Activities method set (like crashActivities
// in crash_resume_test.go) but never blocks: both captured histories must
// run to completion quickly and deterministically. judgeScore controls
// whether the single judge passes (>= 4, ships) or fails (< 4, rejects) the
// one blocking axis.
type captureActivities struct {
	judgeScore int
}

func (a *captureActivities) Prepare(ctx context.Context, in JobInput) (Workspace, error) {
	return Workspace{Path: "/tmp/tollgate-capture"}, nil
}

func (a *captureActivities) RunAgent(ctx context.Context, in RunAgentInput) (AgentResult, error) {
	return AgentResult{CostUSD: 1.5, Output: "captured diff"}, nil
}

func (a *captureActivities) RecordCosts(ctx context.Context, entries []ports.CostEntry) error {
	return nil
}

func (a *captureActivities) LoadRubric(ctx context.Context, path string) (gate.Rubric, error) {
	return captureRubric, nil
}

func (a *captureActivities) JudgeOne(ctx context.Context, in JudgeInput) (ports.Judgment, error) {
	return ports.Judgment{
		Verdict: gate.Verdict{
			Judge:         in.Model,
			RubricVersion: in.Rubric.Version,
			Scores:        map[string]int{"correctness": a.judgeScore},
		},
	}, nil
}

func (a *captureActivities) DecideGate(ctx context.Context, in DecideInput) (gate.Decision, error) {
	return gate.Decide(in.Rubric, in.Verdicts)
}

func (a *captureActivities) Ship(ctx context.Context, ws Workspace) (ShipResult, error) {
	return ShipResult{PRURL: "https://github.com/Benja272/tollgate/pull/0"}, nil
}

// TestCaptureJobWorkflowHistories (re)captures the two JobWorkflow replay
// fixtures — ship and reject — at the base commit, before any production
// line of the artifact-jobs change exists. It is opt-in (TOLLGATE_CAPTURE_HISTORY=1)
// and a no-op otherwise: this file must never fail a normal test run, and
// re-running it against a changed engine is meaningless (see Task 13, which
// replays these frozen fixtures against the FINAL code instead).
func TestCaptureJobWorkflowHistories(t *testing.T) {
	if os.Getenv("TOLLGATE_CAPTURE_HISTORY") == "" {
		t.Skip("history capture is opt-in; set TOLLGATE_CAPTURE_HISTORY=1 to (re)capture the base-commit JobWorkflow histories")
	}

	if _, err := exec.LookPath("temporal"); err != nil {
		t.Fatalf("the temporal CLI is required to capture history as JSON: %v", err)
	}

	c, err := client.Dial(client.Options{})
	require.NoError(t, err, "capture requires a reachable Temporal dev server (temporal server start-dev)")
	defer c.Close()

	outDir := "testdata"
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	captureOne(t, c, "ship", &captureActivities{judgeScore: 5}, JobInput{
		JobID:       "capture-ship",
		Repo:        "Benja272/tollgate",
		SourceRef:   "issue-capture-ship",
		Prompt:      "capture fixture: ship",
		JudgeModels: []string{"haiku"},
	}, StatusShipped, filepath.Join(outDir, "jobworkflow_ship.history.json"))

	captureOne(t, c, "reject", &captureActivities{judgeScore: 2}, JobInput{
		JobID:          "capture-reject",
		Repo:           "Benja272/tollgate",
		SourceRef:      "issue-capture-reject",
		Prompt:         "capture fixture: reject",
		JudgeModels:    []string{"haiku"},
		MaxFixAttempts: -1, // no fix loop: the first blocking FAIL is terminal.
	}, StatusRejected, filepath.Join(outDir, "jobworkflow_reject.history.json"))
}

// captureOne runs JobWorkflow to completion on an ephemeral task queue, then
// shells out to the temporal CLI to dump the full event history as JSON —
// the exact format go.temporal.io/sdk's WorkflowReplayer.ReplayWorkflowHistoryFromJSONFile
// expects (the SDK's own JSON codec for history protos lives in an
// unimportable internal package; the CLI's `--output json` is the
// documented, supported way to produce this file).
func captureOne(t *testing.T, c client.Client, name string, acts interface{}, in JobInput, want JobStatus, outPath string) {
	t.Helper()

	taskQueue := fmt.Sprintf("tollgate-capture-%s-%d", name, time.Now().UnixNano())
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(JobWorkflow)
	w.RegisterActivity(acts)
	require.NoError(t, w.Start())
	defer w.Stop()

	workflowID := fmt.Sprintf("tollgate-capture-%s-%d", name, time.Now().UnixNano())
	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: taskQueue,
	}, JobWorkflow, in)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result JobResult
	require.NoError(t, run.Get(ctx, &result), "capture run %q did not complete", name)
	require.Equal(t, want, result.Status, "capture run %q reached the wrong terminal status", name)

	out, err := exec.Command("temporal", "workflow", "show",
		"--workflow-id", workflowID,
		"--output", "json",
	).CombinedOutput()
	require.NoError(t, err, "temporal CLI history capture for %q failed: %s", name, out)

	var probe json.RawMessage
	require.NoError(t, json.Unmarshal(out, &probe), "captured history for %q is not valid JSON:\n%s", name, out)

	require.NoError(t, os.WriteFile(outPath, out, 0o644))
	t.Logf("captured %s history: %s (%d bytes)", name, outPath, len(out))
}
