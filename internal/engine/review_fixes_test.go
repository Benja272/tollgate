package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/Benja272/tollgate/internal/adapters/claudecode"
	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/workspace"
)

// appErrTypes walks an error tree (including multi-%w wraps) and returns the
// type of every temporal.ApplicationError in it, outermost first.
func appErrTypes(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if appErr, ok := e.(*temporal.ApplicationError); ok {
			out = append(out, appErr.Type())
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

func requireNonRetryableType(t *testing.T, err error, wantType string) {
	t.Helper()
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, wantType, appErr.Type(), "error: %v", err)
	require.True(t, appErr.NonRetryable(), "%s must reach Temporal as non-retryable", wantType)
}

// activityStarts counts attempts per activity type in a test environment.
type activityStarts struct {
	mu     sync.Mutex
	counts map[string]int
	infos  map[string]activity.Info
}

func watchActivities(env *testsuite.TestWorkflowEnvironment) *activityStarts {
	s := &activityStarts{counts: map[string]int{}, infos: map[string]activity.Info{}}
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.counts[info.ActivityType.Name]++
		s.infos[info.ActivityType.Name] = *info
	})
	return s
}

func (s *activityStarts) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[name]
}

func (s *activityStarts) info(t *testing.T, name string) activity.Info {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.infos[name]
	require.True(t, ok, "activity %s never started", name)
	return info
}

// validatingRunner is an AgentRunner that also validates configs, the way
// claudecode.Runner does, with a scripted outcome. It enforces the model
// requirement itself, as the adapter must, and records what it was asked.
type validatingRunner struct {
	model       string
	validateErr error
	run         func(ctx context.Context) (ports.RunResult, error)
	runs        atomic.Int32
	gotReq      ports.AgentConfigRequirements
	bound       time.Duration
}

func (r *validatingRunner) ValidateConfig(_ json.RawMessage, req ports.AgentConfigRequirements) error {
	r.gotReq = req
	if r.validateErr != nil {
		return r.validateErr
	}
	if req.RequireModel && r.model == "" {
		return errors.Join(ports.ErrInvalidAgentConfig, errors.New("model required"))
	}
	return nil
}

func (r *validatingRunner) ShutdownBound() time.Duration { return r.bound }

func (r *validatingRunner) Run(ctx context.Context, _ ports.RunSpec) (ports.RunResult, error) {
	r.runs.Add(1)
	if r.run == nil {
		return ports.RunResult{CostUSD: 0.1, Model: r.model, Output: "ok"}, nil
	}
	return r.run(ctx)
}

type memLedger struct {
	mu      sync.Mutex
	entries []ports.CostEntry
}

func (l *memLedger) RecordCosts(_ context.Context, entries []ports.CostEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entries...)
	return nil
}

// countingCheckout is a Checkout port that counts calls and fails with err.
type countingCheckout struct {
	calls atomic.Int32
	err   error
}

func (c *countingCheckout) Checkout(context.Context, string, string, string) error {
	c.calls.Add(1)
	return c.err
}

func realActivitiesEnv(t *testing.T, acts *Activities) (*testsuite.TestWorkflowEnvironment, *activityStarts) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(acts)
	return env, watchActivities(env)
}

func int64Sum(t *testing.T, tel recordingTelemetry, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, tel.reader.Collect(context.Background(), &rm))
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %q is not an int64 sum", name)
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

func passRubric() gate.Rubric {
	return gate.Rubric{Name: "test", Version: "sha256:abc", Axes: []gate.Axis{{Name: "correctness", Blocking: true, MinScore: 4}}}
}

func passJudgment() ports.Judgment {
	return ports.Judgment{Verdict: passVerdict(passRubric().Version), CostUSD: 0.1}
}

func passDecision() gate.Decision {
	return gate.Decision{Outcome: gate.OutcomePass, Policy: gate.PolicyFailClosedV1, RubricVersion: passRubric().Version}
}

// --- B10: port sentinels reach Temporal as non-retryable -------------------

func TestArtifactJobWorkflow_CheckoutSentinels_NonRetryableSingleAttempt(t *testing.T) {
	cases := map[error]string{
		ports.ErrCheckoutConflict: errTypeCheckoutConflict,
		ports.ErrInvalidRepo:      errTypeInvalidRepo,
		ports.ErrRefNotFound:      errTypeRefNotFound,
	}
	for sentinel, wantType := range cases {
		t.Run(wantType, func(t *testing.T) {
			checkout := &countingCheckout{err: errors.Join(sentinel, errors.New("detail"))}
			acts := &Activities{
				Agent: &validatingRunner{model: "haiku"}, Checkout: checkout,
				Ledger: &memLedger{}, WorkspaceRoot: t.TempDir(),
			}
			env, starts := realActivitiesEnv(t, acts)

			env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())

			require.True(t, env.IsWorkflowCompleted())
			requireNonRetryableType(t, env.GetWorkflowError(), wantType)
			require.Equal(t, int32(1), checkout.calls.Load(), "a non-retryable checkout failure must not be retried")
			require.Equal(t, 1, starts.count("CheckoutWorkspace"))
			require.Zero(t, starts.count("RunAgent"))
		})
	}
}

func TestArtifactJobWorkflow_OverlaySentinels_NonRetryableSingleAttempt(t *testing.T) {
	cases := []struct {
		name     string
		wantType string
		prepare  func(t *testing.T, ws string, in *ArtifactJobInput)
	}{
		{
			name:     "missing source",
			wantType: errTypeOverlayUnsupportedSource,
			prepare: func(t *testing.T, ws string, in *ArtifactJobInput) {
				in.Overlays = []workspace.Overlay{{Source: filepath.Join(t.TempDir(), "absent"), Dest: "output/x"}}
			},
		},
		{
			name:     "symlinked root",
			wantType: errTypeOverlayOutsideRoots,
			prepare: func(t *testing.T, ws string, in *ArtifactJobInput) {
				require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(ws, "output")))
				src := filepath.Join(t.TempDir(), "x")
				require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
				in.Overlays = []workspace.Overlay{{Source: src, Dest: "output/x"}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			in := validArtifactInput()
			ws := filepath.Join(root, "tollgate-artifact-"+in.JobID)
			require.NoError(t, os.MkdirAll(ws, 0o755))
			tc.prepare(t, ws, &in)

			runner := &validatingRunner{model: "haiku"}
			acts := &Activities{
				Agent: runner, Checkout: &countingCheckout{},
				Ledger: &memLedger{}, WorkspaceRoot: root,
			}
			env, starts := realActivitiesEnv(t, acts)

			env.ExecuteWorkflow(ArtifactJobWorkflow, in)

			require.True(t, env.IsWorkflowCompleted())
			requireNonRetryableType(t, env.GetWorkflowError(), tc.wantType)
			require.Equal(t, 1, starts.count("ApplyOverlay"), "a non-retryable overlay failure must not be retried")
			require.Zero(t, runner.runs.Load())
		})
	}
}

func TestArtifactJobWorkflow_InvalidAgentConfig_FailsBeforeCheckout(t *testing.T) {
	cases := map[string]*validatingRunner{
		"config rejected by the adapter": {validateErr: errors.Join(ports.ErrInvalidAgentConfig, errors.New("unknown field"))},
		"no model named":                 {model: ""},
	}
	for name, runner := range cases {
		t.Run(name, func(t *testing.T) {
			checkout := &countingCheckout{}
			acts := &Activities{Agent: runner, Checkout: checkout, Ledger: &memLedger{}, WorkspaceRoot: t.TempDir()}
			env, starts := realActivitiesEnv(t, acts)

			env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())

			require.True(t, env.IsWorkflowCompleted())
			requireNonRetryableType(t, env.GetWorkflowError(), errTypeInvalidAgentConfig)
			require.Equal(t, 1, starts.count("ValidateAgentConfig"))
			require.Zero(t, checkout.calls.Load(), "a bad config must fail before any checkout")
			require.Zero(t, runner.runs.Load())
			require.True(t, runner.gotReq.RequireModel, "the artifact-job requirement is enforced by the adapter")
		})
	}
}

// runAgentWithRetriesWorkflow runs RunAgent under a generous retry budget,
// so a test can prove an error type is never retried.
func runAgentWithRetriesWorkflow(ctx workflow.Context, in RunAgentInput) (AgentResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3, InitialInterval: time.Millisecond},
	})
	var acts *Activities
	var out AgentResult
	err := workflow.ExecuteActivity(ctx, acts.RunAgent, in).Get(ctx, &out)
	return out, err
}

func TestActivities_RunAgent_ErrorClasses_AttemptsThroughTemporal(t *testing.T) {
	cases := []struct {
		name         string
		runErr       error
		wantType     string
		wantAttempts int32
	}{
		{"invalid agent config", errors.Join(ports.ErrInvalidAgentConfig, errors.New("bad")), errTypeInvalidAgentConfig, 1},
		{"unmetered run", &ports.UnmeteredRunError{Err: context.DeadlineExceeded}, errTypeAgentRunUnmetered, 1},
		{"ambiguous envelope", &ports.UnmeteredRunError{Err: ports.ErrAmbiguousEnvelope}, errTypeAgentRunAmbiguousEnvelope, 1},
		{"unsupported platform", fmt.Errorf("claude code: %w", errors.ErrUnsupported), errTypeUnsupportedPlatform, 1},
		{"billed run", &ports.RunError{Result: ports.RunResult{CostUSD: 0.3}, Err: errors.New("is_error")}, errTypeAgentRunBilled, 1},
		{"plain crash stays retryable", errors.New("exec: claude: not found"), "", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &validatingRunner{run: func(context.Context) (ports.RunResult, error) {
				return ports.RunResult{}, tc.runErr
			}}
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(runAgentWithRetriesWorkflow)
			env.RegisterActivity(&Activities{Agent: runner, HeartbeatInterval: time.Hour})

			env.ExecuteWorkflow(runAgentWithRetriesWorkflow, RunAgentInput{JobID: "job-r", Attempt: 1})

			require.True(t, env.IsWorkflowCompleted())
			require.Equal(t, tc.wantAttempts, runner.runs.Load())
			if tc.wantType != "" {
				requireNonRetryableType(t, env.GetWorkflowError(), tc.wantType)
			} else {
				require.Error(t, env.GetWorkflowError())
			}
		})
	}
}

// --- B6: unmetered runs ------------------------------------------------------

func TestActivities_RunAgent_Unmetered_LoggedAndCounted(t *testing.T) {
	tel := newRecordingTelemetry(t)
	acts := &Activities{
		Agent: &validatingRunner{run: func(context.Context) (ports.RunResult, error) {
			return ports.RunResult{}, &ports.UnmeteredRunError{Err: errors.New("signal: killed")}
		}},
		HeartbeatInterval: time.Hour,
		Telemetry:         tel.inst,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{JobID: "job-u", Attempt: 1})

	requireNonRetryableType(t, err, errTypeAgentRunUnmetered)
	require.Equal(t, int64(1), int64Sum(t, tel, "tollgate.agent.unmetered_runs"))
}

func TestActivities_RunAgent_MarginDerivedFromRunnerShutdownBound(t *testing.T) {
	var agentDeadline time.Time
	acts := &Activities{
		Agent: &validatingRunner{bound: 500 * time.Millisecond, run: func(ctx context.Context) (ports.RunResult, error) {
			agentDeadline, _ = ctx.Deadline()
			<-ctx.Done()
			return ports.RunResult{}, &ports.UnmeteredRunError{Err: ctx.Err()}
		}},
		HeartbeatInterval: time.Hour,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	activityDeadline, _ := ctx.Deadline()

	_, err := acts.RunAgent(ctx, RunAgentInput{JobID: "job-d", Attempt: 1})

	requireNonRetryableType(t, err, errTypeAgentRunUnmetered)
	require.NoError(t, ctx.Err(), "the activity must return before its own deadline, or the server retries regardless")
	require.WithinDuration(t, activityDeadline.Add(-(500*time.Millisecond + runReturnSlack)), agentDeadline, 10*time.Millisecond)
}

// The margin must cover the adapter's whole shutdown path — group kill,
// WaitDelay for a process that escaped the group and still holds stdout,
// envelope parse, returning the error — or the server times the attempt out
// first and retries it. This pins that with the real runner.
func TestActivities_RunAgent_RealRunnerShutdownFinishesBeforeActivityDeadline(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("setsid not available: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escaped.pid")
	bin := filepath.Join(dir, "claude")
	// The escaped child leaves the process group, so the group kill cannot
	// close its copy of stdout: only WaitDelay ends the wait.
	script := "#!/bin/sh\nsetsid sh -c 'echo $$ > " + pidFile + "; exec sleep 60' &\nexec sleep 60\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	acts := &Activities{
		Agent:             &claudecode.Runner{Bin: bin, WaitDelay: 3 * time.Second},
		HeartbeatInterval: time.Hour,
	}
	// One second of agent time on top of the derived margin: a margin that
	// ignored the WaitDelay would still be shutting down at the deadline.
	timeout := 3*time.Second + runReturnSlack + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	_, err := acts.RunAgent(ctx, RunAgentInput{JobID: "job-s", Workspace: Workspace{Path: dir}, Prompt: "render", Attempt: 1})
	elapsed := time.Since(start)

	requireNonRetryableType(t, err, errTypeAgentRunUnmetered)
	require.NoError(t, ctx.Err(), "shutdown took %s of a %s activity budget", elapsed, timeout)
	require.GreaterOrEqual(t, elapsed, time.Second+3*time.Second, "the WaitDelay path was exercised")
}

// --- B9: billed failures reach telemetry, details stay small ---------------

func TestActivities_RunAgent_Billed_SpanCarriesCostAndModel_DetailsOmitOutput(t *testing.T) {
	tel := newRecordingTelemetry(t)
	billed := ports.RunResult{CostUSD: 0.8, Model: "claude-haiku-4-5", Output: strings.Repeat("x", 1<<20)}
	acts := &Activities{
		Agent: &validatingRunner{run: func(context.Context) (ports.RunResult, error) {
			return ports.RunResult{}, &ports.RunError{Result: billed, Err: errors.New("is_error")}
		}},
		HeartbeatInterval: time.Hour,
		Telemetry:         tel.inst,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{JobID: "job-b", Attempt: 1})

	requireNonRetryableType(t, err, errTypeAgentRunBilled)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	var details AgentResult
	require.NoError(t, appErr.Details(&details))
	require.Empty(t, details.Output, "the agent's output must not ride on the failure payload")
	require.InDelta(t, 0.8, details.CostUSD, 1e-9)

	ended := tel.spans.Ended()
	require.Len(t, ended, 1)
	attrs := spanAttrs(t, ended[0])
	require.Equal(t, "claude-haiku-4-5", attrs["gen_ai.response.model"].AsString())
	require.InDelta(t, 0.8, attrs["tollgate.cost.usd"].AsFloat64(), 1e-9)
}

// --- billed path in the workflow helper -------------------------------------

func TestRunAgentAndRecord_BilledDetailsUndecodable_FailsLoudly(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var acts *Activities
	boom := temporal.NewNonRetryableApplicationError("billed", errTypeAgentRunBilled, nil, "not an AgentResult")
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, boom)
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(runAgentAndRecordTestWorkflow, RunAgentInput{JobID: "job-x", Attempt: 1})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	require.Contains(t, err.Error(), "spend NOT recorded")
	requireNonRetryableType(t, err, errTypeAgentRunBilledUnrecorded)
	require.Contains(t, appErrTypes(err), errTypeAgentRunBilled)
	env.AssertNotCalled(t, "RecordCosts", mock.Anything, mock.Anything)
}

func TestRunAgentAndRecord_BilledThenLedgerFails_ReportsBothErrors(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var acts *Activities
	boom := temporal.NewNonRetryableApplicationError("agent is_error", errTypeAgentRunBilled, nil, AgentResult{CostUSD: 0.4})
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, boom)
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
		Return(temporal.NewNonRetryableApplicationError("ledger down", "LedgerDown", nil))

	env.ExecuteWorkflow(runAgentAndRecordTestWorkflow, RunAgentInput{JobID: "job-y", Attempt: 1})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	requireNonRetryableType(t, err, errTypeAgentRunBilledUnrecorded)
	require.Contains(t, appErrTypes(err), errTypeAgentRunBilled, "the agent failure must stay in the chain")
	require.Contains(t, err.Error(), "ledger down", "the ledger failure must stay visible")
}

// --- validate() warnings ----------------------------------------------------

func TestArtifactJobWorkflow_InvalidInput_ReviewAdditions(t *testing.T) {
	cases := map[string]func(in *ArtifactJobInput){
		"relative repo":            func(in *ArtifactJobInput) { in.Repo = "repo" },
		"empty repo":               func(in *ArtifactJobInput) { in.Repo = "" },
		"empty prompt":             func(in *ArtifactJobInput) { in.Prompt = "" },
		"whitespace prompt":        func(in *ArtifactJobInput) { in.Prompt = " \n\t" },
		"whitespace piece id":      func(in *ArtifactJobInput) { in.PieceID = "   " },
		"negative agent timeout":   func(in *ArtifactJobInput) { in.AgentTimeoutMinutes = -1 },
		"agent timeout over a day": func(in *ArtifactJobInput) { in.AgentTimeoutMinutes = 24*60 + 1 },
		// time.Duration(m)*time.Minute wraps for these; the bounds check must
		// run on the minutes, before multiplying (review R2).
		"timeout wrapping to zero":     func(in *ArtifactJobInput) { in.AgentTimeoutMinutes = 1 << 53 },
		"timeout wrapping to a minute": func(in *ArtifactJobInput) { in.AgentTimeoutMinutes = 1<<53 + 1 },
		"max int timeout":              func(in *ArtifactJobInput) { in.AgentTimeoutMinutes = math.MaxInt },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			in := validArtifactInput()
			mutate(&in)

			env.ExecuteWorkflow(ArtifactJobWorkflow, in)

			require.True(t, env.IsWorkflowCompleted())
			requireInvalidInput(t, env.GetWorkflowError())
			env.AssertNotCalled(t, "ValidateAgentConfig", mock.Anything, mock.Anything)
			env.AssertNotCalled(t, "CheckoutWorkspace", mock.Anything, mock.Anything)
		})
	}
}

// --- activity options the replay test cannot see ---------------------------

func mockArtifactActivities(env *testsuite.TestWorkflowEnvironment) {
	var acts *Activities
	env.OnActivity(acts.ValidateAgentConfig, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.CheckoutWorkspace, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/a"}, nil)
	env.OnActivity(acts.ApplyOverlay, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{CostUSD: 1, Model: "m"}, nil)
}

func TestArtifactJobWorkflow_AgentTimeout_DefaultAndOverride(t *testing.T) {
	cases := map[string]struct {
		minutes int
		want    time.Duration
	}{
		"default suits a render": {0, 60 * time.Minute},
		"override":               {90, 90 * time.Minute},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			starts := watchActivities(env)
			mockArtifactActivities(env)
			var acts *Activities
			env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(nil)
			in := validArtifactInput()
			in.AgentTimeoutMinutes = tc.minutes

			env.ExecuteWorkflow(ArtifactJobWorkflow, in)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			info := starts.info(t, "RunAgent")
			require.Equal(t, tc.want, info.StartToCloseTimeout)
			require.Equal(t, 5*time.Second, info.HeartbeatTimeout)
		})
	}
}

func TestJobWorkflow_ActivityOptions_Unchanged(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	starts := watchActivities(env)

	var acts *Activities
	env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/j"}, nil)
	env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).Return(passRubric(), nil)
	env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{CostUSD: 1, Model: "m"}, nil)
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.JudgeOne, mock.Anything, mock.Anything).Return(passJudgment(), nil)
	env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).Return(passDecision(), nil)
	env.OnActivity(acts.Ship, mock.Anything, mock.Anything).Return(ShipResult{}, nil)

	env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-o", Prompt: "p", JudgeModels: []string{"haiku"}})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	agent := starts.info(t, "RunAgent")
	require.Equal(t, 10*time.Minute, agent.StartToCloseTimeout)
	require.Equal(t, 5*time.Second, agent.HeartbeatTimeout)

	for _, name := range []string{"Prepare", "LoadRubric", "RecordCosts", "JudgeOne", "DecideGate", "Ship"} {
		info := starts.info(t, name)
		require.Equal(t, 10*time.Minute, info.StartToCloseTimeout, name)
		require.Zero(t, info.HeartbeatTimeout, name)
	}
}

// The test environment does not expose an activity's retry policy, so the
// retry caps are asserted by behavior: a retryable failure is attempted
// exactly as often as the policy allows.
func TestWorkflows_RetryCaps(t *testing.T) {
	crash := errors.New("agent process crashed")
	cases := map[string]struct {
		setup    func(env *testsuite.TestWorkflowEnvironment)
		run      func(env *testsuite.TestWorkflowEnvironment)
		activity string
		want     int
	}{
		"JobWorkflow RunAgent": {
			setup: func(env *testsuite.TestWorkflowEnvironment) {
				var acts *Activities
				env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/j"}, nil)
				env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).Return(passRubric(), nil)
				env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, crash)
			},
			run: func(env *testsuite.TestWorkflowEnvironment) {
				env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-c", Prompt: "p"})
			},
			activity: "RunAgent", want: 2,
		},
		"JobWorkflow Prepare uses the server default (unbounded) policy": {
			setup: func(env *testsuite.TestWorkflowEnvironment) {
				var acts *Activities
				calls := 0
				env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(
					func(context.Context, JobInput) (Workspace, error) {
						calls++
						if calls < 5 {
							return Workspace{}, crash
						}
						return Workspace{}, temporal.NewNonRetryableApplicationError("stop", "Stop", nil)
					})
			},
			run: func(env *testsuite.TestWorkflowEnvironment) {
				env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-p", Prompt: "p"})
			},
			activity: "Prepare", want: 5,
		},
		"ArtifactJobWorkflow RunAgent": {
			setup: func(env *testsuite.TestWorkflowEnvironment) {
				var acts *Activities
				env.OnActivity(acts.ValidateAgentConfig, mock.Anything, mock.Anything).Return(nil)
				env.OnActivity(acts.CheckoutWorkspace, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/a"}, nil)
				env.OnActivity(acts.ApplyOverlay, mock.Anything, mock.Anything).Return(nil)
				env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, crash)
			},
			run: func(env *testsuite.TestWorkflowEnvironment) {
				env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())
			},
			activity: "RunAgent", want: 2,
		},
		"ArtifactJobWorkflow CheckoutWorkspace": {
			setup: func(env *testsuite.TestWorkflowEnvironment) {
				var acts *Activities
				env.OnActivity(acts.ValidateAgentConfig, mock.Anything, mock.Anything).Return(nil)
				env.OnActivity(acts.CheckoutWorkspace, mock.Anything, mock.Anything).Return(Workspace{}, crash)
			},
			run: func(env *testsuite.TestWorkflowEnvironment) {
				env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())
			},
			activity: "CheckoutWorkspace", want: 3,
		},
		"ArtifactJobWorkflow ApplyOverlay": {
			setup: func(env *testsuite.TestWorkflowEnvironment) {
				var acts *Activities
				env.OnActivity(acts.ValidateAgentConfig, mock.Anything, mock.Anything).Return(nil)
				env.OnActivity(acts.CheckoutWorkspace, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/a"}, nil)
				env.OnActivity(acts.ApplyOverlay, mock.Anything, mock.Anything).Return(crash)
			},
			run: func(env *testsuite.TestWorkflowEnvironment) {
				env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())
			},
			activity: "ApplyOverlay", want: 3,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			starts := watchActivities(env)
			tc.setup(env)

			tc.run(env)

			require.True(t, env.IsWorkflowCompleted())
			require.Error(t, env.GetWorkflowError())
			require.Equal(t, tc.want, starts.count(tc.activity))
		})
	}
}

// Every cost row carries the Temporal run id, so two executions of the same
// JobID keep separate rows (ADR-0006 §9).
func TestWorkflows_CostRowsCarryRunID(t *testing.T) {
	t.Run("ArtifactJobWorkflow", func(t *testing.T) {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		mockArtifactActivities(env)
		var recorded []ports.CostEntry
		var acts *Activities
		env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) { recorded = append(recorded, args.Get(1).([]ports.CostEntry)...) }).
			Return(nil)

		env.ExecuteWorkflow(ArtifactJobWorkflow, validArtifactInput())

		require.NoError(t, env.GetWorkflowError())
		require.Len(t, recorded, 1)
		require.Equal(t, testRunID, recorded[0].RunID)
	})
	t.Run("JobWorkflow", func(t *testing.T) {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		var acts *Activities
		env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/j"}, nil)
		env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).Return(passRubric(), nil)
		env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{CostUSD: 1, Model: "m"}, nil)
		env.OnActivity(acts.JudgeOne, mock.Anything, mock.Anything).Return(passJudgment(), nil)
		env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).Return(passDecision(), nil)
		env.OnActivity(acts.Ship, mock.Anything, mock.Anything).Return(ShipResult{}, nil)
		var recorded []ports.CostEntry
		env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) { recorded = append(recorded, args.Get(1).([]ports.CostEntry)...) }).
			Return(nil)

		env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job-r", Prompt: "p", JudgeModels: []string{"haiku"}})

		require.NoError(t, env.GetWorkflowError())
		require.Len(t, recorded, 2, "one run_agent row and one judge row")
		for _, e := range recorded {
			require.Equal(t, testRunID, e.RunID, "%s row", e.Phase)
		}
	})
}

// --- R1: the row is keyed by the run that PAID -------------------------------

func TestRunAgent_ResultAndBilledDetailsCarryThePayingRunID(t *testing.T) {
	for name, runner := range map[string]*validatingRunner{
		"success": {model: "m"},
		"billed failure": {run: func(context.Context) (ports.RunResult, error) {
			return ports.RunResult{}, &ports.RunError{Result: ports.RunResult{CostUSD: 0.3}, Err: errors.New("is_error")}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestActivityEnvironment()
			acts := &Activities{Agent: runner, HeartbeatInterval: time.Hour}
			env.RegisterActivity(acts.RunAgent)

			val, err := env.ExecuteActivity(acts.RunAgent, RunAgentInput{JobID: "j", Attempt: 1})

			var got AgentResult
			if err == nil {
				require.NoError(t, val.Get(&got))
			} else {
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				require.NoError(t, appErr.Details(&got))
			}
			require.NotEmpty(t, got.PaidByRunID, "the activity must report the run that paid")
		})
	}
}

func TestRecordCosts_UsesThePayingRunID_FallsBackToTheWritingRun(t *testing.T) {
	cases := map[string]struct{ paid, want string }{
		"paid by an earlier run": {"run-that-paid", "run-that-paid"},
		"legacy result":          {"", testRunID},
	}
	for name, tc := range cases {
		t.Run(name+"/success", func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			var acts *Activities
			env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).
				Return(AgentResult{CostUSD: 1, Model: "m", PaidByRunID: tc.paid}, nil)
			var recorded []ports.CostEntry
			env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) { recorded = append(recorded, args.Get(1).([]ports.CostEntry)...) }).
				Return(nil)

			env.ExecuteWorkflow(runAgentAndRecordTestWorkflow, RunAgentInput{JobID: "j", Attempt: 1})

			require.NoError(t, env.GetWorkflowError())
			require.Len(t, recorded, 1)
			require.Equal(t, tc.want, recorded[0].RunID)
		})
		t.Run(name+"/billed failure", func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			var acts *Activities
			boom := temporal.NewNonRetryableApplicationError("billed", errTypeAgentRunBilled, nil,
				AgentResult{CostUSD: 1, PaidByRunID: tc.paid})
			env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{}, boom)
			var recorded []ports.CostEntry
			env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) { recorded = append(recorded, args.Get(1).([]ports.CostEntry)...) }).
				Return(nil)

			env.ExecuteWorkflow(runAgentAndRecordTestWorkflow, RunAgentInput{JobID: "j", Attempt: 1})

			require.Error(t, env.GetWorkflowError())
			require.Len(t, recorded, 1)
			require.Equal(t, tc.want, recorded[0].RunID)
		})
		t.Run(name+"/judge", func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			var acts *Activities
			env.OnActivity(acts.Prepare, mock.Anything, mock.Anything).Return(Workspace{Path: "/tmp/j"}, nil)
			env.OnActivity(acts.LoadRubric, mock.Anything, mock.Anything).Return(passRubric(), nil)
			env.OnActivity(acts.RunAgent, mock.Anything, mock.Anything).Return(AgentResult{CostUSD: 1, Model: "m", PaidByRunID: tc.paid}, nil)
			judgment := passJudgment()
			judgment.PaidByRunID = tc.paid
			env.OnActivity(acts.JudgeOne, mock.Anything, mock.Anything).Return(judgment, nil)
			env.OnActivity(acts.DecideGate, mock.Anything, mock.Anything).Return(passDecision(), nil)
			env.OnActivity(acts.Ship, mock.Anything, mock.Anything).Return(ShipResult{}, nil)
			var recorded []ports.CostEntry
			env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) { recorded = append(recorded, args.Get(1).([]ports.CostEntry)...) }).
				Return(nil)

			env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "j", Prompt: "p", JudgeModels: []string{"haiku"}})

			require.NoError(t, env.GetWorkflowError())
			require.Len(t, recorded, 2)
			for _, e := range recorded {
				require.Equal(t, tc.want, e.RunID, "%s row", e.Phase)
			}
		})
	}
}

func TestJudgeOne_JudgmentCarriesThePayingRunID(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	acts := &Activities{Judges: map[string]ports.Judge{"haiku": passingJudge{cost: 0.1}}}
	env.RegisterActivity(acts.JudgeOne)

	val, err := env.ExecuteActivity(acts.JudgeOne, JudgeInput{JobID: "j", Model: "haiku", Rubric: passRubric()})
	require.NoError(t, err)

	var got ports.Judgment
	require.NoError(t, val.Get(&got))
	require.NotEmpty(t, got.PaidByRunID)
}

func TestArtifactJobWorkflow_AgentTimeout_UpperBoundAccepted(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	starts := watchActivities(env)
	mockArtifactActivities(env)
	var acts *Activities
	env.OnActivity(acts.RecordCosts, mock.Anything, mock.Anything).Return(nil)
	in := validArtifactInput()
	in.AgentTimeoutMinutes = 1440

	env.ExecuteWorkflow(ArtifactJobWorkflow, in)

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 24*time.Hour, starts.info(t, "RunAgent").StartToCloseTimeout)
}

// A budget below the kill margin refuses the run before anything starts:
// nothing was spent, so it is not an unmetered run.
func TestActivities_RunAgent_BudgetBelowMargin_RefusedNothingSpentNotCounted(t *testing.T) {
	tel := newRecordingTelemetry(t)
	runner := &validatingRunner{bound: 5 * time.Second}
	acts := &Activities{Agent: runner, HeartbeatInterval: time.Hour, Telemetry: tel.inst}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := acts.RunAgent(ctx, RunAgentInput{JobID: "job-b", Attempt: 1})

	requireNonRetryableType(t, err, errTypeAgentRunBudgetTooShort)
	require.Zero(t, runner.runs.Load(), "the agent must not start")
	require.Zero(t, int64Sum(t, tel, "tollgate.agent.unmetered_runs"))
}

// recordingLogger captures log calls from the Temporal test environment.
type recordingLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	level, msg string
	fields     map[string]any
}

func (l *recordingLogger) log(level, msg string, keyvals []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fields := map[string]any{}
	for i := 0; i+1 < len(keyvals); i += 2 {
		if k, ok := keyvals[i].(string); ok {
			fields[k] = keyvals[i+1]
		}
	}
	l.entries = append(l.entries, logEntry{level, msg, fields})
}

func (l *recordingLogger) Debug(msg string, kv ...any) { l.log("debug", msg, kv) }
func (l *recordingLogger) Info(msg string, kv ...any)  { l.log("info", msg, kv) }
func (l *recordingLogger) Warn(msg string, kv ...any)  { l.log("warn", msg, kv) }
func (l *recordingLogger) Error(msg string, kv ...any) { l.log("error", msg, kv) }

func TestActivities_RunAgent_Unmetered_LogCarriesJobAndAttempt(t *testing.T) {
	logger := &recordingLogger{}
	var ts testsuite.WorkflowTestSuite
	ts.SetLogger(logger)
	env := ts.NewTestActivityEnvironment()
	acts := &Activities{
		Agent: &validatingRunner{run: func(context.Context) (ports.RunResult, error) {
			return ports.RunResult{}, &ports.UnmeteredRunError{Err: errors.New("signal: killed")}
		}},
		HeartbeatInterval: time.Hour,
	}
	env.RegisterActivity(acts.RunAgent)

	_, err := env.ExecuteActivity(acts.RunAgent, RunAgentInput{JobID: "job-logged", Attempt: 3})
	requireNonRetryableType(t, err, errTypeAgentRunUnmetered)

	logger.mu.Lock()
	defer logger.mu.Unlock()
	var found bool
	for _, e := range logger.entries {
		if e.level == "error" && strings.Contains(e.msg, "unmetered agent run") {
			found = true
			require.Equal(t, "job-logged", e.fields["job_id"])
			require.Equal(t, int32(3), e.fields["attempt"])
		}
	}
	require.True(t, found, "the unmetered run must be logged")
}

func TestActivities_RunAgent_AmbiguousEnvelope_CountedLikeUnmetered(t *testing.T) {
	tel := newRecordingTelemetry(t)
	acts := &Activities{
		Agent: &validatingRunner{run: func(context.Context) (ports.RunResult, error) {
			return ports.RunResult{}, &ports.UnmeteredRunError{Err: fmt.Errorf("two envelopes: %w", ports.ErrAmbiguousEnvelope)}
		}},
		HeartbeatInterval: time.Hour,
		Telemetry:         tel.inst,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{JobID: "job-a", Attempt: 1})

	requireNonRetryableType(t, err, errTypeAgentRunAmbiguousEnvelope)
	require.Equal(t, int64(1), int64Sum(t, tel, "tollgate.agent.unmetered_runs"))
}
