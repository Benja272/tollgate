# Tollgate

[![ci](https://github.com/Benja272/tollgate/actions/workflows/ci.yml/badge.svg)](https://github.com/Benja272/tollgate/actions/workflows/ci.yml)

**Metering and quality gates for coding agents you don't own.**

Coding agents open PRs; nobody tells you what the run cost or blocks the PR
when the result isn't good enough. Tollgate wraps external coding agents
(Claude Code headless first) on a durable workflow engine and adds the two
missing pieces:

- **A cost ledger per job** — every ticket→PR run, itemized by phase, judge,
  retry, and model. Not per API call: per unit of work you actually pay for.
- **A pre-PR quality gate** — N independent judges score the diff against a
  versioned rubric *before* the PR exists. Fail → no PR, full report.

Why both, in one number: on SWE-bench (bash-only, 2026) one model scores
75.8% at $36.60 while another scores 76.8% at $377. A 10x cost spread at
indistinguishable quality — invisible without per-job metering, unusable
without a gate that lets you take the cheap option safely.

## Telemetry: why there is a custom cost attribute

Every paid call — the agent run and each judge — emits an OpenTelemetry
`invoke_agent` span following the GenAI semantic conventions, with tokens
under `gen_ai.usage.*` and the model under `gen_ai.request.model`.

Cost is not in those conventions. The GenAI attribute registry defines token
counts, models, and providers, and **no cost attribute at all** — which is
exactly the gap this project is about. Tollgate therefore emits its own,
namespaced so it can never collide with a future registry attribute:

```
tollgate.cost.usd = 1.75    # float64 USD, alongside gen_ai.* on the same span
```

The same number is emitted as a metric (`tollgate_cost_usd_total` in
Prometheus), split by actor, phase, and model. The naming, the pinned semconv
version, and the risk that these conventions are Development-stability and now
live in a separate repository are documented in
[ADR-0005](docs/adr/0005-custom-cost-attribute.md).

A local stack that shows it — collector, Prometheus, and a provisioned Grafana
dashboard, all as code — is one command away:

```sh
docker compose -f deploy/docker-compose.observability.yml up -d
```

See [`deploy/README.md`](deploy/README.md) for the panels, the configuration,
and why metrics are emitted by the worker instead of derived from spans.

Status: design phase. See `docs/DESIGN.md` and `docs/adr/`.
