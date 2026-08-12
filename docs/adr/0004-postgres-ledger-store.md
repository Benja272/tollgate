# ADR-0004: PostgreSQL as the ledger store

Date: 2026-08-12
Status: accepted

## Context

The ledger needs durable, queryable persistence. Cost data already exists in
Temporal's event history (every activity result carries its cost), but the
journal is per-workflow, base64-encoded, and retention-limited — it answers
"what happened in this job", never "what did judges cost this month". The
ledger is the cross-job analytical view.

A fair challenge was raised during design: if the ledger is one append-only
table, is a database server justified? SQLite would suffice for that shape.

## Decision

PostgreSQL, one instance, owning the full domain schema.

## Rationale

- **It is not one table.** The data model (DESIGN.md §3) has six entities —
  Job, Phase, CostEntry, Rubric, Verdict, GateDecision — with relational
  queries across them. Verdicts and decisions must outlive the journal's
  retention for ADR-0003 replay to stay possible.
- **Concurrent writers.** N judge activities per job write cost entries in
  parallel, from potentially many workers. PostgreSQL's MVCC handles this
  natively; SQLite serializes writers behind a file lock.
- **Zero marginal footprint.** Production Temporal requires a database and
  recommends PostgreSQL; the deployment track (week 6) operates one
  instance either way. The ledger rides on it. Choosing SQLite would mean
  operating two persistence systems instead of one.
- **Hiring signal**, an explicit project goal (see ADR-0001): schema design,
  migrations, and aggregation queries on PostgreSQL transfer directly to
  the platform roles this project targets.

## Alternatives considered

- **SQLite**: technically sufficient at side-project volume and the right
  default for single-writer embedded persistence. Rejected here on
  concurrent writers, the shared-instance argument, and transfer value —
  not on capability at current scale.
- **Keep it in the journal**: already exists, but per-workflow, encoded,
  and expiring. Rejected as the system of record; it remains the source
  the ledger is written from.
- **Columnar/analytical stores** (ClickHouse, DuckDB): built for volumes
  and query shapes this project will not reach. Revisit only if ledger
  analytics ever dominate.

## Consequences

- Local dev runs PostgreSQL in Docker (`tollgate-postgres`); CI adds a
  postgres service container.
- Schema changes are goose migrations under `migrations/`, applied
  explicitly — never hand-edited schemas.
- Money lands in `NUMERIC`, never floating point, even though agent costs
  are client-side estimates: the ledger must add up exactly.
- The ledger write path lives behind a port (`ports.LedgerStore`) like
  every other side effect.
