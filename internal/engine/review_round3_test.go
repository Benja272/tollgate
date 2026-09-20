package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"

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
