# Local observability stack

Everything the tollgate worker's telemetry needs, as code: an OpenTelemetry
Collector, Prometheus, and a provisioned Grafana with the dashboard committed
in this repo.

```sh
docker compose -f deploy/docker-compose.observability.yml up -d
go run ./cmd/worker            # no telemetry config needed: default endpoint
```

| Service | URL | What it is |
|---|---|---|
| Collector (OTLP/HTTP) | http://localhost:4318 | the worker's default export target |
| Collector (OTLP/gRPC) | localhost:4317 | for other producers |
| Collector metrics | http://localhost:8889/metrics | what Prometheus scrapes |
| Prometheus | http://localhost:9090 | 7-day local retention |
| Grafana | http://localhost:3000 | anonymous admin, dashboard "Tollgate — cost and gate quality" |

## How metrics reach Prometheus

```
worker ──OTLP/HTTP──► collector ──/metrics──◄ scrape ── Prometheus ──► Grafana
 spans + metrics        (:4318)      (:8889)                             (:3000)
```

The worker emits **metrics directly**, next to its spans, rather than having
the collector derive them from spans with the `spanmetrics` connector.

That is the simpler *and* the more robust path here, for one decisive reason:
spanmetrics produces call counts and latency histograms from span duration.
It cannot sum an arbitrary span attribute, and every number this project
exists to show — dollars spent, tokens by class, judge scores — is an
attribute value, not a duration. Deriving them would mean a
transform/`connector` chain that turns attributes into metrics inside the
collector: more moving parts, all of them silently breakable, to reproduce
three instrument calls the worker can make itself in-process.

Emitting directly also keeps the collector replaceable: point
`OTEL_EXPORTER_OTLP_ENDPOINT` anywhere that speaks OTLP and the same metrics
arrive.

## What the worker emits

Spans (one per paid model invocation, GenAI conventions, semconv v1.40.0 —
ADR-0005):

| Span | Attributes |
|---|---|
| `invoke_agent coding-agent` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.agent.name`, `gen_ai.usage.*_tokens`, `tollgate.cost.usd`, `tollgate.job.id`, `tollgate.phase`, `tollgate.actor` |
| `invoke_agent judge:<model>` | the same, plus `gen_ai.request.model` |

Metrics:

| Metric | Prometheus name | Attributes |
|---|---|---|
| `tollgate.cost.usd` (counter, USD) | `tollgate_cost_usd_total` | `tollgate.actor`, `tollgate.phase`, `gen_ai.request.model` |
| `gen_ai.client.token.usage` (histogram) | `gen_ai_client_token_usage_{sum,count,bucket}` | the above plus `gen_ai.token.type` ∈ input, output, cache_read, cache_creation |
| `tollgate.judge.score` (histogram, 1..5) | `tollgate_judge_score_{sum,count,bucket}` | the above plus `tollgate.rubric.axis`, `tollgate.rubric.version` |

## Dashboard panels

`grafana/dashboards/tollgate-cost.json`, provisioned into the "Tollgate"
folder:

1. Total cost in range, total tokens in range, average judge score (stats).
2. Cost rate by actor (USD/hour) — agent vs each judge model.
3. Cost by actor over the range.
4. Cost per job, top 10 — **opt-in**, see below.
5. Tokens by class (input / output / cache_read / cache_creation).
6. Judge score distribution over the rubric's 1..5 scale.
7. Average judge score by model and axis — the calibration view.

## Configuration

| Variable | Default | Effect |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4318` | OTLP/HTTP base URL; `/v1/traces` and `/v1/metrics` are appended |
| `TOLLGATE_METRICS_JOB_ID` | `false` | adds `tollgate.job.id` to **metrics** |

### Why job id is off by default

Job ids are unbounded, and an unbounded metric label is how a Prometheus
instance dies — one series per job, forever. Spans always carry
`tollgate.job.id` (bounded by trace retention, not by series count), and the
per-job cost of record lives in the PostgreSQL ledger (ADR-0004), which is
built exactly for that question.

The flag exists because at local-stack scale a per-job cost panel is genuinely
useful and genuinely cheap. Turn it on for a demo or a small deployment; leave
it off in anything long-lived, or drop the label in the collector.

### If the collector is down

Nothing happens to the jobs. The worker never dials the collector at startup,
export failures are dropped after a bounded retry, and the failure is logged
at most once a minute. Telemetry is not load-bearing anywhere in tollgate.

## Traces

The collector logs traces (`debug` exporter) rather than storing them, which
keeps the default stack to three containers. To keep them, add Tempo:

```yaml
# docker-compose.observability.yml
  tempo:
    image: grafana/tempo:2.9.0
    command: ["-config.file=/etc/tempo.yaml"]
    volumes: ["./tempo.yaml:/etc/tempo.yaml:ro"]
```

then point the traces pipeline at it (`exporters: [otlp/tempo]` with
`endpoint: tempo:4317`) and add a Tempo datasource next to the Prometheus one
in `grafana/provisioning/datasources/`.
