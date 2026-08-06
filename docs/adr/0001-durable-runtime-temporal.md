# ADR-0001: Temporal as the durable execution runtime

Date: 2026-08-06
Status: accepted

## Context

A tollgate job (ticket → agent run → judges → gate → PR) spends real money on
external agent calls and ends in an irreversible side effect (PR creation).
The runtime must guarantee: crash-resume mid-job, exactly-once effect
semantics around paid calls and PR creation, durable timers, and human
approval signals. Candidates: Temporal, Restate, DBOS.

## Decision

Temporal, via its Go SDK.

## Rationale

- **Journal-replay with exactly-once activity semantics** — checkpointing
  models (re-run the step, hope it's idempotent) are the wrong default when a
  step shells out to a paid agent or opens a PR.
- **Worker rescheduling**: a job survives the death of the worker running it,
  not just the process restart.
- **Ecosystem signal**: the employers this project targets name Temporal in
  their own job postings (Railway billing/scalability, PostHog billing).
  Design conversations transfer directly.
- **Mature Go SDK** with activity retry policies, heartbeats, and signals.

## Alternatives considered

- **Restate**: strongest challenger — first-class Go SDK, journal-based
  exactly-once, single binary (no cluster). Rejected on ecosystem maturity
  and hiring-signal grounds, not on technical ones. Revisit if operating
  Temporal proves disproportionate to the project's size.
- **DBOS**: Postgres-backed library, no server to run. Attractive
  operationally, but workflow versioning and signal semantics are younger,
  and the library model couples orchestration to the service process.
- **LangGraph (+ Temporal)**: no Go implementation; its checkpointing re-runs
  nodes from the start on resume (idempotency is the developer's problem, per
  its own docs). The composition makes sense when you author the agent loop —
  tollgate orchestrates opaque external agents, so there is no graph to
  author, only a pipeline: plain Temporal activities.

## Consequences

- Requires a Temporal server in every environment (dev: `temporal server
  start-dev`; prod: provisioned via Terraform — see the deployment track).
- Workflow determinism rules apply (no direct I/O in workflow code; all side
  effects in activities). This constraint is also the teaching goal.
