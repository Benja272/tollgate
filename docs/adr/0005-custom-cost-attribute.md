# ADR-0005: `tollgate.cost.usd`, a custom cost attribute

Date: 2026-08-13
Status: accepted

## Context

Tollgate exists to answer "what did this job cost". Its telemetry therefore
has to carry money, and the natural home for that is the OpenTelemetry GenAI
semantic conventions, which tollgate already follows for the rest of a model
call.

The registry does not have a home for it. As of the conventions shipped with
`go.opentelemetry.io/otel` v1.45.0 (schema **1.40.0**, the last version whose
generated Go package still contains the `gen_ai` namespace), the GenAI
attribute registry defines:

- `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`,
  `gen_ai.usage.cache_read.input_tokens`,
  `gen_ai.usage.cache_creation.input_tokens` — quantities,
- `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.provider.name`,
  `gen_ai.operation.name`, `gen_ai.agent.name` — identity,
- and **no cost attribute at all**, in any form: not USD, not a currency
  code, not a generic `gen_ai.usage.cost`.

That is defensible upstream — price tables are vendor-specific, change without
notice, and a client-side estimate is not billing truth — and it is precisely
the gap this project was built around (DESIGN.md §1). Tokens are the ground
truth; the money figure is the thing the user actually pays and the thing no
vendor exposes per job.

## Decision

Emit cost as a custom attribute and metric named **`tollgate.cost.usd`**, a
float64 of US dollars, alongside the conventional `gen_ai.*` attributes on
every `invoke_agent` span and on the cost counter.

Related tollgate-namespaced attributes, same rationale:
`tollgate.job.id`, `tollgate.phase`, `tollgate.actor`,
`tollgate.rubric.axis`, `tollgate.rubric.version`, `tollgate.judge.score`.

Pinned semconv: `go.opentelemetry.io/otel/semconv/v1.40.0` (plus its
`genaiconv` subpackage), from `go.opentelemetry.io/otel` v1.45.0.

## Rationale

- **Own namespace, not a squatted one.** Everything tollgate invents sits
  under `tollgate.`, never under `gen_ai.`. If the registry later defines a
  cost attribute, our name cannot collide with it, cannot shadow it, and the
  migration is a rename with a documented old name — not an ambiguity about
  whose semantics a `gen_ai.*` key carries.
- **Unit in the name.** `usd`, not `cost` with a separate currency attribute.
  A single-currency float is what the harness reports (`total_cost_usd` from
  Claude Code), and encoding the unit in the name makes a dashboard query
  unambiguous and a future multi-currency attribute an explicit new name
  rather than a silent meaning change.
- **Estimate, stated as such.** The value is the agent harness's client-side
  estimate, not an invoice. The ledger (ADR-0004) stores the same number in
  `NUMERIC` as the record; telemetry is the observable view of it, and both
  are documented as estimates.
- **The cost metric is not derivable from spans.** The collector's
  spanmetrics connector produces call counts and latency histograms; it
  cannot sum an arbitrary span attribute. Cost therefore has to be a metric
  the worker emits directly (see `deploy/README.md`).

### The token-type extension

`gen_ai.token.type` is an open enum whose registry values are `input` and
`output`. Anthropic bills two more classes — cache reads and cache creation —
and they dominate agent runs. Tollgate records them as
`gen_ai.token.type=cache_read` and `cache_creation` on the conventional
`gen_ai.client.token.usage` instrument, rather than inventing a parallel
tollgate metric: extending an open enum keeps one instrument, one query, and
one obvious upgrade path if the registry names those classes itself.

## Risks

- **The conventions are Development stability.** GenAI conventions are
  explicitly unstable: names, values, and span shapes may change between
  schema versions with no compatibility guarantee. Anything we build on them
  can break on a semconv bump.
- **They have already moved.** The GenAI conventions were split out of the
  main semantic-conventions repository into a dedicated
  `semantic-conventions-genai` project, and the effect is visible in the Go
  packages: `go.opentelemetry.io/otel` ships generated semconv packages up to
  v1.43.0, but the `gen_ai` namespace disappears after **v1.40.0**. The
  conventions are not gone — they are governed and released elsewhere, on a
  different cadence, and the Go constants for them will eventually come from
  a different module.
- **Consequence of both:** the pin is deliberate, not incidental. Tollgate
  imports `semconv/v1.40.0` explicitly, and a bump is a reviewed change with
  a dashboard check, never a transitive side effect of upgrading the SDK.
- **Our own attribute is stable by contrast.** `tollgate.cost.usd` is ours;
  it changes only when this ADR changes, which is exactly why cost is not
  parked in a `gen_ai.*` key.

## Consequences

- Dashboards and queries use `tollgate_cost_usd_total` (the Prometheus
  translation of the counter); the panel descriptions in
  `deploy/grafana/dashboards/tollgate-cost.json` name this ADR.
- A semconv upgrade must re-check three things: that the `gen_ai` namespace
  still exists in the chosen package, that `invoke_agent` is still the
  operation name, and that no upstream cost attribute has appeared. If one
  has, this ADR is superseded by a migration ADR that keeps
  `tollgate.cost.usd` emitted alongside it for at least one release.
- Telemetry stays non-load-bearing: spans and metrics are best-effort, and a
  missing or broken collector never fails a job (`internal/telemetry`, nil
  instruments are a working no-op).
