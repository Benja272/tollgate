// Package telemetry instruments the parts of tollgate that spend money.
//
// Spans follow the OpenTelemetry GenAI semantic conventions (Development
// stability, pinned at semconv v1.40.0 — see docs/adr/0005), with one
// documented custom attribute: `tollgate.cost.usd`. The GenAI registry
// defines token attributes but no cost attribute at all, and cost is the
// question this project exists to answer.
//
// Telemetry is never load-bearing: a nil *Instruments is a working no-op, so
// an unwired or misconfigured worker keeps running jobs.
package telemetry

import (
	"context"
	"errors"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/Benja272/tollgate/internal/ports"
)

// ScopeName identifies this instrumentation scope in exported telemetry.
const ScopeName = "github.com/Benja272/tollgate/internal/telemetry"

// Attribute keys tollgate defines itself, all under the `tollgate.` prefix so
// they can never collide with a future registry attribute (ADR-0005).
const (
	// AttrCostUSD is the cost of one paid model invocation, in US dollars.
	// It has no GenAI-registry equivalent; that gap is why it exists.
	AttrCostUSD = attribute.Key("tollgate.cost.usd")
	// AttrJobID ties every span of one job together. It stays off metrics by
	// default — see WithJobIDMetricAttribute.
	AttrJobID = attribute.Key("tollgate.job.id")
	// AttrPhase is the workflow phase: run_agent, judge, ...
	AttrPhase = attribute.Key("tollgate.phase")
	// AttrActor is the ledger actor: agent, judge:<model>, fixer.
	AttrActor = attribute.Key("tollgate.actor")
	// AttrRubricAxis and AttrRubricVersion pin a judge score to what it scored.
	AttrRubricAxis    = attribute.Key("tollgate.rubric.axis")
	AttrRubricVersion = attribute.Key("tollgate.rubric.version")
)

// Metric names. Token usage rides the conventional GenAI instrument; cost and
// judge scores have no conventional instrument, so they are namespaced.
const (
	MetricCostUSD    = "tollgate.cost.usd"
	MetricJudgeScore = "tollgate.judge.score"
	MetricTokenUsage = "gen_ai.client.token.usage"
)

// gen_ai.token.type is an open enum; the registry enumerates only input and
// output. Anthropic bills two more classes, and omitting them makes a reported
// cost inexplicable, so tollgate extends the enum rather than inventing a
// parallel metric.
const (
	tokenTypeCacheRead     genaiconv.TokenTypeAttr = "cache_read"
	tokenTypeCacheCreation genaiconv.TokenTypeAttr = "cache_creation"
)

// defaultProvider is the only GenAI provider tollgate adapts today (ADR-0002:
// Claude Code). Call.Provider overrides it once a second adapter exists.
var defaultProvider = genaiconv.ProviderNameAnthropic

// Views returns the metric views tollgate needs. Judge scores live on the
// rubric's 1..5 scale, which the SDK's latency-shaped default buckets would
// collapse into two buckets.
func Views() []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: MetricJudgeScore},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{1, 2, 3, 4, 5},
			}},
		),
	}
}

// Call identifies one paid model invocation.
type Call struct {
	JobID string
	// Phase is the workflow phase the call belongs to (run_agent, judge).
	Phase string
	// Actor matches the ledger's actor (agent, judge:<model>), so a span and
	// its cost row are joinable.
	Actor string
	// AgentName is gen_ai.agent.name and the span-name suffix.
	AgentName string
	// Model is gen_ai.request.model. Empty when the harness picks the model
	// and does not report it back (the Claude Code agent run).
	Model string
	// Provider is gen_ai.provider.name; empty means defaultProvider.
	Provider genaiconv.ProviderNameAttr
}

// Result is what one call reported back: what it consumed and what it cost.
type Result struct {
	Usage   ports.TokenUsage
	CostUSD float64
}

// Instruments holds the tracer and the instruments every paid call records to.
type Instruments struct {
	tracer trace.Tracer
	cost   metric.Float64Counter
	tokens genaiconv.ClientTokenUsage
	score  metric.Int64Histogram

	jobIDOnMetrics bool
}

// Option configures Instruments.
type Option func(*Instruments)

// WithJobIDMetricAttribute adds tollgate.job.id to metric attributes.
//
// Off by default on purpose: job ids are unbounded, and unbounded metric
// labels are how a Prometheus instance dies. Per-job cost is answered
// authoritatively by the ledger (ADR-0004) and traceably by spans; enable this
// only for a small local stack where a per-job metric panel is worth it.
func WithJobIDMetricAttribute() Option {
	return func(i *Instruments) { i.jobIDOnMetrics = true }
}

// New builds Instruments from the given providers. Both are required — the
// no-telemetry case is a nil *Instruments, not a half-wired one.
func New(tp trace.TracerProvider, mp metric.MeterProvider, opts ...Option) (*Instruments, error) {
	if tp == nil || mp == nil {
		return nil, errors.New("telemetry: tracer and meter providers are required")
	}
	meter := mp.Meter(ScopeName)

	cost, costErr := meter.Float64Counter(MetricCostUSD,
		metric.WithDescription("Cost of paid model invocations, as reported by the agent harness."),
		// An annotation-only unit: it documents the dimension without adding a
		// unit suffix to the exported Prometheus metric name.
		metric.WithUnit("{USD}"),
	)
	tokens, tokensErr := genaiconv.NewClientTokenUsage(meter)
	score, scoreErr := meter.Int64Histogram(MetricJudgeScore,
		metric.WithDescription("Judge scores per rubric axis."),
		metric.WithUnit("{score}"),
	)
	if err := errors.Join(costErr, tokensErr, scoreErr); err != nil {
		return nil, err
	}

	inst := &Instruments{
		tracer: tp.Tracer(ScopeName),
		cost:   cost,
		tokens: tokens,
		score:  score,
	}
	for _, opt := range opts {
		opt(inst)
	}
	return inst, nil
}

// Recording is one in-flight invoke_agent span.
type Recording struct {
	inst *Instruments
	call Call
	span trace.Span
}

// StartInvokeAgent opens the GenAI `invoke_agent` span around one paid call.
// The returned Recording must be ended exactly once.
func (i *Instruments) StartInvokeAgent(ctx context.Context, call Call) (context.Context, *Recording) {
	if i == nil {
		return ctx, nil
	}
	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameInvokeAgent,
		semconv.GenAIProviderNameKey.String(string(call.provider())),
		semconv.GenAIAgentName(call.AgentName),
		AttrJobID.String(call.JobID),
		AttrPhase.String(call.Phase),
		AttrActor.String(call.Actor),
	}
	if call.Model != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(call.Model))
	}

	// Span name per the GenAI conventions: "{operation} {agent name}".
	ctx, span := i.tracer.Start(ctx, "invoke_agent "+call.AgentName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return ctx, &Recording{inst: i, call: call, span: span}
}

// End closes the span, recording usage and cost on success, or the error on
// failure. A failed call reports no usage and no cost: nothing was billed that
// the harness told us about, and inventing a zero-cost data point would dilute
// the cost aggregates.
func (r *Recording) End(ctx context.Context, res Result, err error) {
	if r == nil {
		return
	}
	defer r.span.End()

	if err != nil {
		r.span.RecordError(err)
		r.span.SetStatus(codes.Error, err.Error())
		r.span.SetAttributes(semconv.ErrorType(err))
		return
	}

	r.span.SetAttributes(
		semconv.GenAIUsageInputTokens(int(res.Usage.InputTokens)),
		semconv.GenAIUsageOutputTokens(int(res.Usage.OutputTokens)),
		semconv.GenAIUsageCacheReadInputTokens(int(res.Usage.CacheReadTokens)),
		semconv.GenAIUsageCacheCreationInputTokens(int(res.Usage.CacheCreationTokens)),
		AttrCostUSD.Float64(res.CostUSD),
	)
	r.inst.record(ctx, r.call, res)
}

// record emits the metric side of one completed call.
func (i *Instruments) record(ctx context.Context, call Call, res Result) {
	set := i.metricAttrs(call)
	i.cost.Add(ctx, res.CostUSD, metric.WithAttributes(set...))

	for _, tc := range []struct {
		class genaiconv.TokenTypeAttr
		count int64
	}{
		{genaiconv.TokenTypeInput, res.Usage.InputTokens},
		{genaiconv.TokenTypeOutput, res.Usage.OutputTokens},
		{tokenTypeCacheRead, res.Usage.CacheReadTokens},
		{tokenTypeCacheCreation, res.Usage.CacheCreationTokens},
	} {
		if tc.count <= 0 {
			continue
		}
		i.tokens.Record(ctx, tc.count,
			genaiconv.OperationNameInvokeAgent, call.provider(), tc.class, set...)
	}
}

// RecordJudgeScores records one verdict's per-axis scores, the distribution a
// gate's calibration is read from.
func (i *Instruments) RecordJudgeScores(ctx context.Context, call Call, rubricVersion string, scores map[string]int) {
	if i == nil {
		return
	}
	axes := make([]string, 0, len(scores))
	for axis := range scores {
		axes = append(axes, axis)
	}
	sort.Strings(axes)

	base := i.metricAttrs(call)
	for _, axis := range axes {
		attrs := append(base[:len(base):len(base)],
			AttrRubricAxis.String(axis),
			AttrRubricVersion.String(rubricVersion),
		)
		i.score.Record(ctx, int64(scores[axis]), metric.WithAttributes(attrs...))
	}
}

// metricAttrs is the bounded label set every tollgate metric carries.
func (i *Instruments) metricAttrs(call Call) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		AttrActor.String(call.Actor),
		AttrPhase.String(call.Phase),
	}
	if call.Model != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(call.Model))
	}
	if i.jobIDOnMetrics {
		attrs = append(attrs, AttrJobID.String(call.JobID))
	}
	return attrs
}

func (c Call) provider() genaiconv.ProviderNameAttr {
	if c.Provider == "" {
		return defaultProvider
	}
	return c.Provider
}
