package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/Benja272/tollgate/internal/adapters/postgres"
	"github.com/Benja272/tollgate/internal/ports"
)

// temporalPayloadLimit is the server-side blob size limit an activity
// result or a failure must stay under. Over it the server rejects the
// completion, the attempt fails as if the activity had, and the agent —
// already paid for — is invoked again.
const temporalPayloadLimit = 2 << 20

// bulkyRunner is an agent whose reported output dwarfs Temporal's payload
// limit: a harness that printed its whole transcript into `result`.
type bulkyRunner struct {
	result ports.RunResult
	err    error
}

func (r bulkyRunner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	if r.err != nil {
		return ports.RunResult{}, r.err
	}
	return r.result, nil
}

// payloadSize is how many bytes v occupies once Temporal has encoded it.
func payloadSize(t *testing.T, v any) int {
	t.Helper()
	p, err := converter.GetDefaultDataConverter().ToPayload(v)
	require.NoError(t, err)
	return len(p.GetData())
}

func TestActivities_RunAgent_OversizedOutput_ResultFitsThePayloadLimit(t *testing.T) {
	huge := strings.Repeat("o", 5<<20)
	acts := &Activities{
		Agent: bulkyRunner{result: ports.RunResult{
			CostUSD: 0.42, Model: "claude-haiku-4-5", Output: huge,
			Usage: ports.TokenUsage{InputTokens: 7},
		}},
		HeartbeatInterval: time.Hour,
	}

	got, err := acts.RunAgent(context.Background(), RunAgentInput{
		JobID: "job-oversized", Workspace: Workspace{Path: t.TempDir()}, Prompt: "render",
	})

	require.NoError(t, err)
	require.Less(t, payloadSize(t, got), temporalPayloadLimit,
		"an oversized agent output must not push the activity result over Temporal's limit")
	require.Contains(t, got.Output, "truncated", "the truncation must be explicit in the output")
	require.Contains(t, got.Output, "5242880", "the marker must state how many bytes the agent produced")
	require.True(t, strings.HasPrefix(got.Output, strings.Repeat("o", 1024)), "the kept prefix is the agent's own output")
	require.InDelta(t, 0.42, got.CostUSD, 1e-9, "truncating the output must never touch the cost")
	require.Equal(t, "claude-haiku-4-5", got.Model)
	require.Equal(t, int64(7), got.Usage.InputTokens)
}

func TestActivities_RunAgent_SmallOutput_PassesThroughUntouched(t *testing.T) {
	acts := &Activities{
		Agent:             bulkyRunner{result: ports.RunResult{CostUSD: 0.1, Model: "m", Output: "the whole answer"}},
		HeartbeatInterval: time.Hour,
	}

	got, err := acts.RunAgent(context.Background(), RunAgentInput{Workspace: Workspace{Path: t.TempDir()}, Prompt: "p"})

	require.NoError(t, err)
	require.Equal(t, "the whole answer", got.Output)
}

func TestActivities_RunAgent_OversizedBilledFailure_FailureFitsThePayloadLimit(t *testing.T) {
	huge := strings.Repeat("b", 5<<20)
	acts := &Activities{
		Agent: bulkyRunner{err: &ports.RunError{
			Result: ports.RunResult{CostUSD: 0.07, Model: "claude-haiku-4-5", Output: huge},
			Err:    errors.New(huge),
		}},
		HeartbeatInterval: time.Hour,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{
		JobID: "job-billed-oversized", Workspace: Workspace{Path: t.TempDir()}, Prompt: "render",
	})

	requireNonRetryableType(t, err, errTypeAgentRunBilled)
	require.Less(t, failurePayloadSize(t, err), temporalPayloadLimit,
		"an oversized billed failure must still fit, or the spend in its details is lost and the run is retried")

	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	var details AgentResult
	require.NoError(t, appErr.Details(&details), "the cost details must survive")
	require.InDelta(t, 0.07, details.CostUSD, 1e-9)
	require.Equal(t, "claude-haiku-4-5", details.Model)
}

func TestActivities_RunAgent_OversizedUnmeteredFailure_FailureFitsThePayloadLimit(t *testing.T) {
	huge := strings.Repeat("u", 5<<20)
	acts := &Activities{
		Agent:             bulkyRunner{err: &ports.UnmeteredRunError{Err: errors.New(huge)}},
		HeartbeatInterval: time.Hour,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{
		JobID: "job-unmetered-oversized", Workspace: Workspace{Path: t.TempDir()}, Prompt: "render",
	})

	requireNonRetryableType(t, err, errTypeAgentRunUnmetered)
	require.Less(t, failurePayloadSize(t, err), temporalPayloadLimit,
		"an unmetered failure must reach Temporal, or the run is retried on top of an unknown spend")
}

func TestActivities_RunAgent_OversizedPlainFailure_KeepsItsSentinelAndFits(t *testing.T) {
	huge := strings.Repeat("c", 5<<20)
	acts := &Activities{
		Agent:             bulkyRunner{err: errors.Join(ports.ErrInvalidAgentConfig, errors.New(huge))},
		HeartbeatInterval: time.Hour,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{
		JobID: "job-config-oversized", Workspace: Workspace{Path: t.TempDir()}, Prompt: "render",
	})

	requireNonRetryableType(t, err, errTypeInvalidAgentConfig)
	require.Less(t, failurePayloadSize(t, err), temporalPayloadLimit)
}

// failurePayloadSize is how many bytes err occupies once Temporal has
// converted it into the failure it sends to the server: every message down
// the cause chain, plus the details.
func failurePayloadSize(t *testing.T, err error) int {
	t.Helper()
	failure := temporal.GetDefaultFailureConverter().ErrorToFailure(err)
	data, marshalErr := failure.Marshal()
	require.NoError(t, marshalErr)
	return len(data)
}

// recordingLedger captures what RecordCosts hands the ledger.
type recordingLedger struct {
	mu      sync.Mutex
	entries []ports.CostEntry
	err     error
}

func (l *recordingLedger) RecordCosts(_ context.Context, entries []ports.CostEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.entries = append(l.entries, entries...)
	return nil
}

func (l *recordingLedger) recorded() []ports.CostEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ports.CostEntry(nil), l.entries...)
}

// A deploy can resume a job whose RecordCosts input was journaled before
// run ids existed: that input carries no run id at all. The ledger rejects
// such a row, RecordCosts runs under an unlimited retry policy, and the job
// would retry forever. The run that scheduled the activity is the run that
// paid, which is exactly what the pre-change code keyed rows by.
func TestActivities_RecordCosts_EntryWithoutRunID_KeyedByTheRunningExecution(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()

	ledger := &recordingLedger{}
	acts := &Activities{Ledger: ledger}
	env.RegisterActivity(acts.RecordCosts)
	env.RegisterActivityWithOptions(
		func(ctx context.Context) (string, error) { return activityRunID(ctx), nil },
		activity.RegisterOptions{Name: "reportRunID"},
	)

	val, err := env.ExecuteActivity("reportRunID")
	require.NoError(t, err)
	var wantRunID string
	require.NoError(t, val.Get(&wantRunID))
	require.NotEmpty(t, wantRunID)

	_, err = env.ExecuteActivity(acts.RecordCosts, []ports.CostEntry{{
		JobID: "job-legacy", Phase: "run_agent", Actor: "agent", Model: "m", USD: 0.5, Attempt: 1,
	}})
	require.NoError(t, err, "a legacy entry must be recorded, not retried forever")

	got := ledger.recorded()
	require.Len(t, got, 1)
	require.Equal(t, wantRunID, got[0].RunID, "the row is keyed by the execution that scheduled it")
}

func TestActivities_RecordCosts_ReportedRunID_IsNeverOverwritten(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()

	ledger := &recordingLedger{}
	acts := &Activities{Ledger: ledger}
	env.RegisterActivity(acts.RecordCosts)

	_, err := env.ExecuteActivity(acts.RecordCosts, []ports.CostEntry{{
		JobID: "job-reset", RunID: "the-run-that-paid", Phase: "run_agent", Actor: "agent", USD: 0.5, Attempt: 1,
	}})
	require.NoError(t, err)

	got := ledger.recorded()
	require.Len(t, got, 1)
	require.Equal(t, "the-run-that-paid", got[0].RunID,
		"a row already keyed by the paying run must survive a reset unchanged")
}

// The ledger keeps its own check: outside an activity there is no execution
// to fall back to, and a row without a run id must still be refused.
func TestActivities_RecordCosts_OutsideAnActivity_LedgerStillRejects(t *testing.T) {
	ledger := &recordingLedger{err: errors.New("ledger: cost entry has no run id")}
	acts := &Activities{Ledger: ledger}

	err := acts.RecordCosts(context.Background(), []ports.CostEntry{{JobID: "j", Phase: "run_agent", Actor: "agent"}})

	require.Error(t, err)
}

// The same legacy entry against the real ledger: before the fill, the
// ledger refused the row with ErrMissingRunID and the activity — whose
// retry policy is Temporal's unlimited default — never stopped trying.
func TestActivities_RecordCosts_EntryWithoutRunID_RealLedgerRecordsIt(t *testing.T) {
	ledger := postgres.NewLedger(ledgerPoolOrSkip(t))
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	acts := &Activities{Ledger: ledger}
	env.RegisterActivity(acts.RecordCosts)

	piece := fmt.Sprintf("piece-legacy-runid-%d", time.Now().UnixNano())
	_, err := env.ExecuteActivity(acts.RecordCosts, []ports.CostEntry{{
		JobID:   fmt.Sprintf("job-legacy-%d", time.Now().UnixNano()),
		Phase:   "run_agent",
		Actor:   "agent",
		Model:   "claude-haiku-4-5",
		PieceID: piece,
		USD:     0.5,
		Attempt: 1,
	}})
	require.NoError(t, err, "a pre-change RecordCosts input must be recorded, not rejected forever")

	spend, spendErr := ledger.PerPieceSpend(context.Background(), piece)
	require.NoError(t, spendErr)
	require.Len(t, spend, 1)
	require.InDelta(t, 0.5, spend[0].USD, 1e-9)
}
