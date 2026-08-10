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

Status: design phase. See `docs/DESIGN.md` and `docs/adr/`.
