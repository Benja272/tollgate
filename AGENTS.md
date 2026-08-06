# Tollgate

A metering and quality-gate substrate for coding agents you don't own.

Tollgate wraps external coding agents (Claude Code headless first) on a durable
workflow runtime and owns the two things no agent vendor owns: **what a job
cost** (per ticket, per phase, per judge, per retry) and **whether the result
is good enough to ship** (a multi-judge rubric gate that must pass *before*
a PR is opened).

## What this is not

- Not an agent framework — agents are opaque external processes behind an
  adapter interface. We never author the agent loop.
- Not a re-implementation of permissions/sandboxing — compose the harness's
  own controls (Claude Code permission rules, OS sandboxing) and say so.
- Not a skills registry — the MCP working group owns that race.

## Architecture (summary — see docs/DESIGN.md)

- **Run engine**: Temporal workflow per job. Phases: prepare → run agent →
  judge → gate decision → ship (PR) | reject. Crash-resume is a tested
  invariant, not a hope.
- **Adapter interface**: one Go interface; first implementation shells
  `claude -p --output-format json` (cost comes free via `total_cost_usd`).
- **Ledger**: per-job cost object aggregating every phase, judge call, and
  retry. PostgreSQL. Emitted as OTel spans (GenAI conventions + a documented
  custom cost attribute — the registry has none).
- **Gate**: versioned rubrics, N independent judges, explicit
  disagreement-resolution policy, deterministic replay of any judgment.
  A failing gate blocks PR creation.

## Conventions

- Go, standard project layout, hexagonal boundaries (`internal/ports`,
  `internal/adapters`, `internal/domain`).
- Strict conventional commits. Tests ship with the change.
- Architecture decisions live in `docs/adr/` — read them before proposing
  changes to runtime, adapter, or gate semantics.
- Technical artifacts (code, comments, docs) are in English.
