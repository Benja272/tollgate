package telemetry_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Benja272/tollgate/internal/telemetry"
)

func TestConfigFromEnv_DefaultsToLocalCollector(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	cfg := telemetry.ConfigFromEnv()

	require.Equal(t, "http://localhost:4318", cfg.Endpoint)
	require.Equal(t, telemetry.DefaultServiceName, cfg.ServiceName)
	require.False(t, cfg.JobIDMetricAttribute)
}

func TestConfigFromEnv_ReadsEndpointAndJobIDFlag(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.internal:4318/")
	t.Setenv("TOLLGATE_METRICS_JOB_ID", "true")

	cfg := telemetry.ConfigFromEnv()

	require.Equal(t, "http://collector.internal:4318/", cfg.Endpoint)
	require.True(t, cfg.JobIDMetricAttribute)
}

func TestSignalURL_JoinsWithoutDoublingSlashes(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"http://localhost:4318", "http://localhost:4318/v1/traces"},
		{"http://localhost:4318/", "http://localhost:4318/v1/traces"},
		{"https://otlp.example.com/prefix/", "https://otlp.example.com/prefix/v1/traces"},
	} {
		require.Equal(t, tc.want, telemetry.SignalURL(tc.base, "v1/traces"))
	}
}

func TestSetup_UnreachableCollectorNeverBlocksTheWorker(t *testing.T) {
	// Port 1 is guaranteed closed: this is the "collector is down" case, which
	// must degrade to dropped telemetry, never to a stalled or dead worker.
	cfg := telemetry.Config{ServiceName: "tollgate-worker-test", Endpoint: "http://127.0.0.1:1"}

	start := time.Now()
	inst, shutdown, err := telemetry.Setup(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, inst)
	require.Less(t, time.Since(start), 5*time.Second, "Setup must not wait on the collector")

	_, rec := inst.StartInvokeAgent(context.Background(), telemetry.Call{
		JobID: "job-1", Phase: "run_agent", Actor: "agent", AgentName: "coding-agent",
	})
	rec.End(context.Background(), telemetry.Result{CostUSD: 1}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = shutdown(ctx)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown blocked on an unreachable collector")
	}
}
