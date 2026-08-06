# Tollgate — Design

Status: draft v1 (2026-08-06). Decisions with lasting consequences live in
`docs/adr/`; this document holds the shape of the system and the build plan.

## 1. Problem

Coding agents ship end-to-end (ticket → PR) but no vendor answers two
questions their users pay for daily:

1. **What did this job cost?** Per-call cost is commodity (Langfuse, Helicone,
   LangSmith); nobody models the *job* — one ticket→PR run spanning phases,
   judge calls, and retries — as a first-class costable object. LangSmith's
   own docs concede there is no job/business-entity field. OTel's GenAI
   registry defines token attributes but **no cost attribute at all**.
2. **Was it good enough to ship?** Every vendor's quality gate runs *after*
   the PR is opened (Devin Review, Cursor Bugbot, Codex review). No vendor
   documents a configurable gate that must pass before the agent is allowed
   to open a PR.

Why it matters, in one public number: on SWE-bench's bash-only track,
MiniMax M2.5 scores 75.8% at $36.60 while Opus scores 76.8% at $377 — a 10x
cost spread at statistically indistinguishable quality. Without per-run
metering you cannot see that trade-off; without a pre-PR gate you cannot
safely take the cheap option.

## 2. Shape

```
                       ┌────────────────────────────────────────┐
                       │            Temporal workflow            │
 ticket/issue ──────►  │ prepare ► run agent ► judge ► gate ─┬─► │ ──► PR
                       │    │         │          │          │   │
                       └────┼─────────┼──────────┼──────────┼───┘
                            ▼         ▼          ▼          ▼ reject
                       workspace   adapter    judges     decision
                       (worktree)  (claude -p) (N, rubric) (ledger'd)
                            │         │          │          │
                            └─────────┴────┬─────┴──────────┘
                                           ▼
                                 Ledger (PostgreSQL)
                                 + OTel spans ► Prometheus/Grafana
```

Components:

- **Run engine** — one Temporal workflow per job; activities own all side
  effects. Crash-resume is a tested invariant (kill the worker mid-run in CI;
  the job completes).
- **Agent adapter** — ADR-0002. External process, normalized `RunResult`.
- **Ledger** — the per-job cost object. Every activity that spends money or
  tokens appends a `CostEntry{job, phase, actor, tokens, usd, attempt}`.
  Queryable: cost per job, per phase, per judge, per retry, per model.
- **Gate** — N judges run the versioned rubric independently; a resolution
  policy (default: unanimous-pass for blocking axes, majority for advisory)
  produces a decision that is stored with judge outputs, rubric version, and
  model IDs so any judgment can be replayed deterministically. FAIL → no PR,
  job ends in `rejected` with the full report.
- **API/CLI** — submit a job, watch it, query the ledger.

## 3. Data model (first cut)

- `Job{id, source_ref, repo, status, created_at, decided_at}`
- `Phase{job_id, name, attempt, started_at, ended_at, outcome}`
- `CostEntry{job_id, phase, actor, model, input_tokens, output_tokens,
  usd, attempt}` — `actor` ∈ {agent, judge:<n>, fixer}
- `Rubric{id, version, axes[], blocking[]}` — content-addressed; a gate
  decision pins the exact version it ran.
- `Verdict{job_id, judge, rubric_version, scores{axis: score}, findings[],
  raw_output_ref}`
- `GateDecision{job_id, policy, outcome, verdict_refs[]}`

## 4. Telemetry

Emit OTel GenAI spans (`invoke_agent`, `invoke_workflow` — Development
stability) per phase, plus a **documented custom attribute for cost**
(`tollgate.cost.usd`) since the registry has none — the gap and the naming
rationale get their own ADR and a section in the README. Prometheus scrapes
the service; Grafana dashboards ship in the repo as code.

## 5. Non-goals

- Permissions/sandboxing/risk tiers — fully converged upstream (Claude Code
  permission rules, Factory autonomy ceilings). Compose, don't rebuild.
- Skills registry — MCP working group territory.
- Owning the agent loop — ADR-0002.

## 6. Build plan (6 weeks, part-time)

| Week | Deliverable | Proves |
|---|---|---|
| 1–2 | Run engine + Claude Code adapter; crash-resume test in CI | durable-execution correctness |
| 3–4 | Gate: rubric format, N judges, resolution policy, deterministic replay; failing gate blocks PR | eval as control system |
| 5 | Ledger as first-class object + OTel spans + Grafana dashboard | metering/billing competence |
| 6 | Deployment track: Dockerfile, Terraform (small footprint), Helm/K8s manifests, GitHub Actions CI/CD | IaC/provisioning depth |

Each week ends with something demoable. The README's FAQ answers "why not
Devin / LangGraph / LangSmith" with the specific guarantee and primitive gaps,
sourced.
