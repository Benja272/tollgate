package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/telemetry"
)

// recordingTelemetry wires in-memory OTel recorders, so span assertions need
// no collector and no network.
type recordingTelemetry struct {
	inst   *telemetry.Instruments
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

func newRecordingTelemetry(t *testing.T) recordingTelemetry {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(telemetry.Views()...))
	inst, err := telemetry.New(tp, mp)
	require.NoError(t, err)
	return recordingTelemetry{inst: inst, spans: sr, reader: reader}
}

func spanAttrs(t *testing.T, span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	t.Helper()
	out := make(map[attribute.Key]attribute.Value, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

// costingRunner reports a full usage breakdown, the shape a real agent run has.
type costingRunner struct{}

func (costingRunner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	return ports.RunResult{
		CostUSD: 1.75,
		Usage: ports.TokenUsage{
			InputTokens: 900, OutputTokens: 120,
			CacheReadTokens: 42000, CacheCreationTokens: 3000,
		},
		Output: "diff",
	}, nil
}

type failingRunner struct{}

func (failingRunner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	return ports.RunResult{}, errors.New("claude code run: exit status 1")
}

// scoringJudge returns a fixed verdict plus what producing it cost.
type scoringJudge struct{ model string }

func (j scoringJudge) Judge(ctx context.Context, req ports.JudgeRequest) (ports.Judgment, error) {
	return ports.Judgment{
		Verdict: gate.Verdict{
			Judge:         j.model,
			RubricVersion: req.Rubric.Version,
			Scores:        map[string]int{"correctness": 4},
		},
		CostUSD: 0.03,
		Usage:   ports.TokenUsage{InputTokens: 700, OutputTokens: 60},
	}, nil
}

func TestActivities_RunAgent_RecordsInvokeAgentSpanWithCostAndTokens(t *testing.T) {
	tel := newRecordingTelemetry(t)
	acts := &Activities{
		Agent:             costingRunner{},
		HeartbeatInterval: time.Hour,
		Telemetry:         tel.inst,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{
		JobID:     "job-77",
		Workspace: Workspace{Path: t.TempDir()},
		Prompt:    "implement the ticket",
	})
	require.NoError(t, err)

	ended := tel.spans.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, "invoke_agent coding-agent", ended[0].Name())

	got := spanAttrs(t, ended[0])
	require.Equal(t, "invoke_agent", got["gen_ai.operation.name"].AsString())
	require.Equal(t, "job-77", got["tollgate.job.id"].AsString())
	require.Equal(t, "run_agent", got["tollgate.phase"].AsString())
	require.Equal(t, "agent", got["tollgate.actor"].AsString())
	require.Equal(t, int64(900), got["gen_ai.usage.input_tokens"].AsInt64())
	require.Equal(t, int64(120), got["gen_ai.usage.output_tokens"].AsInt64())
	require.Equal(t, int64(42000), got["gen_ai.usage.cache_read.input_tokens"].AsInt64())
	require.InDelta(t, 1.75, got["tollgate.cost.usd"].AsFloat64(), 1e-9)
}

func TestActivities_RunAgent_FailedRunMarksSpanError(t *testing.T) {
	tel := newRecordingTelemetry(t)
	acts := &Activities{
		Agent:             failingRunner{},
		HeartbeatInterval: time.Hour,
		Telemetry:         tel.inst,
	}

	_, err := acts.RunAgent(context.Background(), RunAgentInput{
		JobID:     "job-78",
		Workspace: Workspace{Path: t.TempDir()},
	})

	require.Error(t, err)
	ended := tel.spans.Ended()
	require.Len(t, ended, 1, "a failed agent run must still close its span")
	require.Equal(t, "Error", ended[0].Status().Code.String())
}

func TestActivities_JudgeOne_RecordsInvokeAgentSpanWithModelAndScores(t *testing.T) {
	tel := newRecordingTelemetry(t)
	acts := &Activities{
		Judges:    map[string]ports.Judge{"haiku": scoringJudge{model: "haiku"}},
		Telemetry: tel.inst,
	}

	_, err := acts.JudgeOne(context.Background(), JudgeInput{
		JobID: "job-77",
		Model: "haiku",
		Rubric: gate.Rubric{
			Version: "sha256:abc",
			Axes:    []gate.Axis{{Name: "correctness", Blocking: true, MinScore: 4}},
		},
	})
	require.NoError(t, err)

	ended := tel.spans.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, "invoke_agent judge:haiku", ended[0].Name())

	got := spanAttrs(t, ended[0])
	require.Equal(t, "haiku", got["gen_ai.request.model"].AsString())
	require.Equal(t, "judge", got["tollgate.phase"].AsString())
	require.Equal(t, "judge:haiku", got["tollgate.actor"].AsString())
	require.InDelta(t, 0.03, got["tollgate.cost.usd"].AsFloat64(), 1e-9)

	var rm metricdata.ResourceMetrics
	require.NoError(t, tel.reader.Collect(context.Background(), &rm))
	require.Equal(t, int64(4), judgeScoreSum(t, rm, "correctness"),
		"judge scores must be observable as a distribution, not only in the verdict")
}

func TestActivities_UnwiredTelemetry_DoesNotBreakActivities(t *testing.T) {
	acts := &Activities{Agent: costingRunner{}, HeartbeatInterval: time.Hour}

	got, err := acts.RunAgent(context.Background(), RunAgentInput{
		JobID:     "job-79",
		Workspace: Workspace{Path: t.TempDir()},
	})

	require.NoError(t, err)
	require.InDelta(t, 1.75, got.CostUSD, 1e-9)
}

func judgeScoreSum(t *testing.T, rm metricdata.ResourceMetrics, axis string) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetry.MetricJudgeScore {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			for _, dp := range hist.DataPoints {
				got, _ := dp.Attributes.Value("tollgate.rubric.axis")
				if got.AsString() == axis {
					total += dp.Sum
				}
			}
		}
	}
	return total
}
