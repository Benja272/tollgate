package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/telemetry"
)

// harness wires the SDK's in-memory recorders so tests observe exactly what a
// collector would receive, without one running.
type harness struct {
	inst   *telemetry.Instruments
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

func newHarness(t *testing.T, opts ...telemetry.Option) harness {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(telemetry.Views()...))

	inst, err := telemetry.New(tp, mp, opts...)
	require.NoError(t, err)
	return harness{inst: inst, spans: sr, reader: reader}
}

func (h harness) collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, h.reader.Collect(context.Background(), &rm))
	return rm
}

// sumFor returns the total of every data point of the named float64 sum.
func sumFor(t *testing.T, rm metricdata.ResourceMetrics, name string) (float64, []attribute.Set) {
	t.Helper()
	var total float64
	var sets []attribute.Set
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[float64])
			require.True(t, ok, "metric %q is not a float64 sum", name)
			for _, dp := range sum.DataPoints {
				total += dp.Value
				sets = append(sets, dp.Attributes)
			}
		}
	}
	return total, sets
}

// histogramFor returns the per-attribute-set sums of the named int64 histogram.
func histogramFor(t *testing.T, rm metricdata.ResourceMetrics, name string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok, "metric %q is not an int64 histogram", name)
			for _, dp := range h.DataPoints {
				key, _ := dp.Attributes.Value("gen_ai.token.type")
				out[key.AsString()] += dp.Sum
			}
		}
	}
	return out
}

func attrs(t *testing.T, span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	t.Helper()
	out := make(map[attribute.Key]attribute.Value, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

func TestInstruments_InvokeAgentSpanCarriesGenAIAndCostAttributes(t *testing.T) {
	h := newHarness(t)

	_, rec := h.inst.StartInvokeAgent(context.Background(), telemetry.Call{
		JobID: "job-7", Phase: "judge", Actor: "judge:haiku",
		AgentName: "judge:haiku", Model: "haiku",
	})
	rec.End(context.Background(), telemetry.Result{
		CostUSD: 0.125,
		Usage: ports.TokenUsage{
			InputTokens: 1200, OutputTokens: 340,
			CacheReadTokens: 40000, CacheCreationTokens: 5000,
		},
	}, nil)

	ended := h.spans.Ended()
	require.Len(t, ended, 1)
	span := ended[0]
	require.Equal(t, "invoke_agent judge:haiku", span.Name())

	got := attrs(t, span)
	require.Equal(t, "invoke_agent", got["gen_ai.operation.name"].AsString())
	require.Equal(t, "anthropic", got["gen_ai.provider.name"].AsString())
	require.Equal(t, "haiku", got["gen_ai.request.model"].AsString())
	require.Equal(t, int64(1200), got["gen_ai.usage.input_tokens"].AsInt64())
	require.Equal(t, int64(340), got["gen_ai.usage.output_tokens"].AsInt64())
	require.Equal(t, int64(40000), got["gen_ai.usage.cache_read.input_tokens"].AsInt64())
	require.Equal(t, int64(5000), got["gen_ai.usage.cache_creation.input_tokens"].AsInt64())
	require.InDelta(t, 0.125, got["tollgate.cost.usd"].AsFloat64(), 1e-9,
		"the GenAI registry has no cost attribute; tollgate.cost.usd is the documented gap-filler (ADR-0005)")
	require.Equal(t, "job-7", got["tollgate.job.id"].AsString())
	require.Equal(t, "judge", got["tollgate.phase"].AsString())
	require.Equal(t, "judge:haiku", got["tollgate.actor"].AsString())
}

func TestInstruments_RecordsCostAndTokensAsMetrics(t *testing.T) {
	h := newHarness(t)

	_, rec := h.inst.StartInvokeAgent(context.Background(), telemetry.Call{
		JobID: "job-7", Phase: "run_agent", Actor: "agent", AgentName: "coding-agent",
	})
	rec.End(context.Background(), telemetry.Result{
		CostUSD: 2.5,
		Usage: ports.TokenUsage{
			InputTokens: 10, OutputTokens: 20,
			CacheReadTokens: 30, CacheCreationTokens: 40,
		},
	}, nil)

	rm := h.collect(t)

	cost, sets := sumFor(t, rm, telemetry.MetricCostUSD)
	require.InDelta(t, 2.5, cost, 1e-9)
	require.Len(t, sets, 1)
	actor, ok := sets[0].Value("tollgate.actor")
	require.True(t, ok)
	require.Equal(t, "agent", actor.AsString())
	_, hasJobID := sets[0].Value("tollgate.job.id")
	require.False(t, hasJobID, "job id must stay off metrics by default: unbounded label cardinality")

	tokens := histogramFor(t, rm, telemetry.MetricTokenUsage)
	require.Equal(t, map[string]int64{
		"input": 10, "output": 20, "cache_read": 30, "cache_creation": 40,
	}, tokens)
}

func TestInstruments_JobIDMetricAttributeIsOptIn(t *testing.T) {
	h := newHarness(t, telemetry.WithJobIDMetricAttribute())

	_, rec := h.inst.StartInvokeAgent(context.Background(), telemetry.Call{
		JobID: "job-9", Phase: "run_agent", Actor: "agent", AgentName: "coding-agent",
	})
	rec.End(context.Background(), telemetry.Result{CostUSD: 1}, nil)

	_, sets := sumFor(t, h.collect(t), telemetry.MetricCostUSD)
	require.Len(t, sets, 1)
	jobID, ok := sets[0].Value("tollgate.job.id")
	require.True(t, ok)
	require.Equal(t, "job-9", jobID.AsString())
}

func TestInstruments_FailedCallMarksSpanErrorAndRecordsNoCost(t *testing.T) {
	h := newHarness(t)

	_, rec := h.inst.StartInvokeAgent(context.Background(), telemetry.Call{
		JobID: "job-7", Phase: "run_agent", Actor: "agent", AgentName: "coding-agent",
	})
	rec.End(context.Background(), telemetry.Result{}, errors.New("agent exited 1"))

	ended := h.spans.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, "Error", ended[0].Status().Code.String())
	require.Len(t, ended[0].Events(), 1, "the failure must be recorded as an exception event")

	cost, _ := sumFor(t, h.collect(t), telemetry.MetricCostUSD)
	require.Zero(t, cost, "a failed call has no reported cost to bill")
}

// A billed failure reported a cost: the span and the cost metric must carry
// it, or telemetry and the ledger disagree on exactly this path (review B9).
func TestRecording_End_BilledError_RecordsCostUsageModelAndMarksError(t *testing.T) {
	h := newHarness(t)

	_, rec := h.inst.StartInvokeAgent(context.Background(), telemetry.Call{
		JobID: "job-8", Phase: "run_agent", Actor: "agent", AgentName: "coding-agent",
	})
	billed := ports.RunResult{CostUSD: 0.9, Model: "claude-haiku-4-5", Usage: ports.TokenUsage{InputTokens: 12, OutputTokens: 3}}
	rec.End(context.Background(),
		telemetry.Result{CostUSD: billed.CostUSD, Usage: billed.Usage, Model: billed.Model},
		&ports.RunError{Result: billed, Err: errors.New("is_error=true")})

	ended := h.spans.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, "Error", ended[0].Status().Code.String())
	got := attrs(t, ended[0])
	require.Equal(t, "claude-haiku-4-5", got["gen_ai.response.model"].AsString())
	require.InDelta(t, 0.9, got["tollgate.cost.usd"].AsFloat64(), 1e-9)
	require.Equal(t, int64(12), got["gen_ai.usage.input_tokens"].AsInt64())

	cost, _ := sumFor(t, h.collect(t), telemetry.MetricCostUSD)
	require.InDelta(t, 0.9, cost, 1e-9, "the billed failure's cost must reach the cost metric")
}

func TestInstruments_RecordUnmeteredRun_CountsPerActorAndPhase(t *testing.T) {
	h := newHarness(t)
	call := telemetry.Call{JobID: "job-8", Phase: "run_agent", Actor: "agent"}

	h.inst.RecordUnmeteredRun(context.Background(), call)
	h.inst.RecordUnmeteredRun(context.Background(), call)

	var total int64
	for _, sm := range h.collect(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetry.MetricUnmeteredRuns {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				total += dp.Value
				actor, _ := dp.Attributes.Value("tollgate.actor")
				require.Equal(t, "agent", actor.AsString())
			}
		}
	}
	require.Equal(t, int64(2), total)

	var nilInst *telemetry.Instruments
	require.NotPanics(t, func() { nilInst.RecordUnmeteredRun(context.Background(), call) })
}

func TestRecording_End_SetsGenAIResponseModelWhenPresent(t *testing.T) {
	h := newHarness(t)

	// The run_agent call site never sets Call.Model up front (the harness
	// picks the model); it only becomes known once the run reports back —
	// exactly like the coding-agent run this exercises (ADR-0006 §7).
	_, rec := h.inst.StartInvokeAgent(context.Background(), telemetry.Call{
		JobID: "job-9", Phase: "run_agent", Actor: "agent", AgentName: "coding-agent",
	})
	rec.End(context.Background(), telemetry.Result{CostUSD: 0.5, Model: "claude-3-5-sonnet"}, nil)

	ended := h.spans.Ended()
	require.Len(t, ended, 1)
	got := attrs(t, ended[0])
	require.Equal(t, "claude-3-5-sonnet", got["gen_ai.response.model"].AsString())
}

func TestRecording_End_OmitsResponseModelWhenEmpty(t *testing.T) {
	h := newHarness(t)

	_, rec := h.inst.StartInvokeAgent(context.Background(), telemetry.Call{
		JobID: "job-9", Phase: "run_agent", Actor: "agent", AgentName: "coding-agent",
	})
	rec.End(context.Background(), telemetry.Result{CostUSD: 0.5}, nil)

	ended := h.spans.Ended()
	require.Len(t, ended, 1)
	got := attrs(t, ended[0])
	_, ok := got["gen_ai.response.model"]
	require.False(t, ok, "an empty resolved model must never be reported as a span attribute")
}

func TestInstruments_RecordJudgeScores(t *testing.T) {
	h := newHarness(t)

	h.inst.RecordJudgeScores(context.Background(),
		telemetry.Call{JobID: "job-7", Phase: "judge", Actor: "judge:haiku", Model: "haiku"},
		"sha256:abc", map[string]int{"correctness": 5, "tests": 3})

	rm := h.collect(t)
	byAxis := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetry.MetricJudgeScore {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			for _, dp := range hist.DataPoints {
				axis, _ := dp.Attributes.Value("tollgate.rubric.axis")
				byAxis[axis.AsString()] = dp.Sum
			}
		}
	}
	require.Equal(t, map[string]int64{"correctness": 5, "tests": 3}, byAxis)
}

func TestInstruments_NilIsNoOp(t *testing.T) {
	var inst *telemetry.Instruments

	ctx, rec := inst.StartInvokeAgent(context.Background(), telemetry.Call{JobID: "job-7"})
	require.NotNil(t, ctx)
	require.NotPanics(t, func() {
		rec.End(context.Background(), telemetry.Result{CostUSD: 1}, nil)
		inst.RecordJudgeScores(context.Background(), telemetry.Call{}, "v", map[string]int{"a": 1})
	}, "telemetry must never be load-bearing: an unwired worker keeps working")
}
