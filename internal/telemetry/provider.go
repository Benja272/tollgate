package telemetry

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// DefaultServiceName is the resource identity the worker reports as.
const DefaultServiceName = "tollgate-worker"

// defaultEndpoint is the OTLP/HTTP endpoint of a collector running next to the
// worker (deploy/docker-compose.observability.yml).
const defaultEndpoint = "http://localhost:4318"

// Export deadlines are deliberately short. Telemetry is best-effort: a slow or
// dead collector must cost a dropped batch, never a stalled worker or a
// shutdown that outlives the process's grace period.
const (
	exportTimeout    = 5 * time.Second
	retryMaxElapsed  = 10 * time.Second
	metricInterval   = 15 * time.Second
	errorLogInterval = time.Minute
)

// Config is the worker's telemetry configuration.
type Config struct {
	// ServiceName lands in the OTel resource as service.name.
	ServiceName string
	// Endpoint is the OTLP/HTTP base URL; signal paths are appended.
	Endpoint string
	// JobIDMetricAttribute enables the high-cardinality job id label on
	// metrics (see WithJobIDMetricAttribute).
	JobIDMetricAttribute bool
}

// ConfigFromEnv reads the standard OTel endpoint variable, falling back to a
// local collector, plus tollgate's own cardinality switch.
func ConfigFromEnv() Config {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	jobID, _ := strconv.ParseBool(os.Getenv("TOLLGATE_METRICS_JOB_ID"))
	return Config{
		ServiceName:          DefaultServiceName,
		Endpoint:             endpoint,
		JobIDMetricAttribute: jobID,
	}
}

// SignalURL appends an OTLP signal path to a base endpoint exactly once.
func SignalURL(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

// Setup installs OTLP/HTTP-exporting tracer and meter providers as the OTel
// globals and returns the instruments activities record to, plus a shutdown
// function to call on exit.
//
// It never dials the collector: OTLP/HTTP connects per export, so a collector
// that is down or absent degrades to dropped batches and a throttled log line.
func Setup(ctx context.Context, cfg Config) (*Instruments, func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = DefaultServiceName
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}

	res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(cfg.ServiceName))

	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(SignalURL(cfg.Endpoint, "v1/traces")),
		otlptracehttp.WithTimeout(exportTimeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 500 * time.Millisecond,
			MaxInterval:     2 * time.Second,
			MaxElapsedTime:  retryMaxElapsed,
		}),
	)
	if err != nil {
		return nil, nil, err
	}

	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(SignalURL(cfg.Endpoint, "v1/metrics")),
		otlpmetrichttp.WithTimeout(exportTimeout),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 500 * time.Millisecond,
			MaxInterval:     2 * time.Second,
			MaxElapsedTime:  retryMaxElapsed,
		}),
	)
	if err != nil {
		return nil, nil, errors.Join(err, traceExp.Shutdown(ctx))
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp, sdktrace.WithExportTimeout(exportTimeout)),
		sdktrace.WithResource(res),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(metricInterval),
			sdkmetric.WithTimeout(exportTimeout),
		)),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(Views()...),
	)

	var opts []Option
	if cfg.JobIDMetricAttribute {
		opts = append(opts, WithJobIDMetricAttribute())
	}
	inst, err := New(tp, mp, opts...)
	if err != nil {
		return nil, nil, errors.Join(err, tp.Shutdown(ctx), mp.Shutdown(ctx))
	}

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	otel.SetErrorHandler(throttledErrorHandler())

	shutdown := func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return inst, shutdown, nil
}

// throttledErrorHandler keeps a permanently unreachable collector from
// drowning the worker's logs: export failures repeat every interval and say
// the same thing every time.
func throttledErrorHandler() otel.ErrorHandler {
	var last atomic.Int64
	return otel.ErrorHandlerFunc(func(err error) {
		now := time.Now().UnixNano()
		prev := last.Load()
		if prev != 0 && now-prev < int64(errorLogInterval) {
			return
		}
		if !last.CompareAndSwap(prev, now) {
			return
		}
		log.Printf("telemetry export failed (best-effort, job execution unaffected): %v", err)
	})
}
