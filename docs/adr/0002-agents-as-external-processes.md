# ADR-0002: Agents are opaque external processes behind an adapter

Date: 2026-08-06
Status: accepted

## Context

Tollgate's value is metering and gating agents it does not own. The
temptation is to integrate deeply with one agent's internals for richer
telemetry.

## Decision

Agents are executed as external processes behind a single Go interface:

```go
type AgentRunner interface {
    // Run executes one agent attempt in the prepared workspace and returns
    // its result, including the agent-reported cost when available.
    Run(ctx context.Context, spec RunSpec) (RunResult, error)
}
```

First implementation: Claude Code headless (`claude -p --output-format json`),
whose JSON output includes `total_cost_usd` and per-model token breakdowns.
Second implementation (proves the abstraction): any OpenHands- or
Codex-compatible CLI.

## Rationale

- The adapter boundary is the product thesis: the layer must survive agent
  vendors changing underneath it.
- Cost reporting differs per agent; the interface normalizes it into the
  ledger's `CostEntry` rather than leaking vendor formats upward.
- Two implementations from early on keep the interface honest.

## Consequences

- Telemetry is limited to what the agent reports plus what tollgate measures
  around it (wall clock, exit codes, workspace diff stats). Acceptable: the
  ledger's unit is the job, not the agent's internals.
- Agent-specific knobs (model, permission mode) live in `RunSpec` as an
  opaque per-adapter config block, not as first-class fields.
