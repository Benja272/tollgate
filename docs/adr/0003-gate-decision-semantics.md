# ADR-0003: Gate decision semantics — disagreement, replay, and the fix loop

Date: 2026-08-10
Status: accepted

## Context

The gate turns N independent judge verdicts into one decision that controls an
irreversible side effect (PR creation). Three questions were unresolved by
DESIGN.md:

1. What happens when judges disagree on a blocking axis?
2. What does "deterministic replay of any judgment" mean, given that LLM
   judges are non-deterministic even at temperature 0 and providers revise
   model snapshots silently?
3. What happens to the agent's work when the gate fails — is it discarded,
   repaired, or handed to a human anyway?

Recent judge-reliability work (arXiv:2606.19544) shows that test-retest
reproducibility can mask systematic bias: a judge with maximal position bias
reproduces its verdict perfectly. Reproducibility and calibration are
separate properties; a gate that conflates them audits well and judges badly.

## Decision

1. **The resolution policy is a pluggable, versioned strategy.** The shipped
   default is fail-closed: unanimous pass required on blocking axes, majority
   on advisory axes. A single blocking FAIL rejects the attempt.
2. **Replay means re-deriving the decision, never re-running judges.** The
   policy is a pure function `(verdicts, rubric_version, policy_version) →
   decision`. All judge outputs are stored raw; replay re-applies the pinned
   policy to the stored verdicts and must reproduce the decision bit-for-bit.
3. **Judge quality is a calibration property.** Each rubric version ships
   with a human-annotated reference set; a judge configuration must reach
   ≥85% agreement with the reference set before it may vote on blocking
   axes. Reproducibility of the decision (point 2) is never accepted as
   evidence of judge quality.
4. **A blocking FAIL triggers the fix loop before terminal rejection.** Judge
   findings are fed back to the agent (the `fixer` actor) for a bounded
   number of repair attempts (default: 2, configurable). Each repaired diff
   is re-judged in full under the same rubric version. Budget exhausted →
   the job ends `rejected` with the complete report.
5. **Every attempt is a first-class immutable record.** Per attempt:
   verdicts, findings, the fixer's diff, and cost entries all persist —
   nothing is overwritten. The attempt history is both the audit trail and
   the raw material for future judge/rubric calibration.
6. **Judge independence constraint.** At least one blocking-axis judge must
   run on a different model family from the agent that produced the diff;
   otherwise failure modes correlate and N judges collapse into one.

Alternative policies ship behind configuration, never as defaults:

- **Tiebreaker**: on a blocking-axis split, one extra judge (different model)
  scores only the disputed axes; the resulting majority decides.
- **Human escalation**: on a blocking-axis split, the workflow pauses on a
  Temporal signal and waits for an operator decision (durable timer bounds
  the wait).
- **Draft-PR delivery**: a FAIL publishes a *draft* PR carrying the gate
  report instead of ending in `rejected`. Never a ready-for-review PR — a
  failed attempt must not enter the human review queue.

## Rationale

- **Fail-closed default**: at fleet scale the scarce resource is human review
  attention. A false rejection costs an agent retry (cents, visible in the
  ledger); a false pass costs a human review — the exact cost the product
  exists to prevent. The asymmetry decides the default.
- **Pure-function replay** is the only achievable determinism. Re-running a
  judge and expecting identical output fails on sampling nondeterminism and
  on silent snapshot revisions; no storage discipline fixes that.
- **The fix loop** delegates repair to the cheapest capable actor. Handing a
  known-failed diff to a human converts the gate into a notification system;
  discarding it wastes paid work. Feeding findings back to the agent is the
  only option that preserves both autonomy and the investment.
- **Immutable attempt history** turns operations into evaluation data: each
  fail→fix→verdict cycle is a labeled example for measuring judge agreement
  drift and rubric quality over time.

## Alternatives considered

- **Replay by re-running judges**: rejected as technically impossible (see
  above). Anyone needing fresh judgments runs a new attempt, which is
  recorded as such.
- **Open the PR on FAIL** ("the human decides the merge anyway"): rejected
  as a default — it reproduces the post-PR review model (Devin Review,
  Cursor Bugbot, Codex review) and abandons the pre-PR gate thesis. Survives
  as the draft-PR delivery policy.
- **Human escalation as default**: rejected — with an immature rubric,
  disagreements are frequent, and pausing on each one makes the operator the
  bottleneck the pipeline exists to remove. Kept as a configurable policy
  for high-stakes repositories.
- **One stronger judge instead of N**: rejected — a single judge is a single
  point of systematic bias (position, verbosity, self-family leniency), and
  no model choice removes the correlation problem with the authoring agent.

## Consequences

- `Verdict` and `GateDecision` gain an `attempt` dimension; `GateDecision`
  pins `policy` and `policy_version` alongside `rubric_version`.
- The fix loop writes `CostEntry{actor: fixer, attempt}` rows from week 1 of
  the gate's existence — the ledger write path must exist before the fix
  loop does, which pulls a minimal `CostEntry` store into the run-engine
  milestone (weeks 1–2), ahead of the week-5 ledger milestone.
- The calibration reference set is a deliverable of the gate milestone
  (weeks 3–4), not an afterthought: without it, no judge may vote on
  blocking axes, so the gate cannot ship.
- The human-escalation policy requires Temporal signals and durable timers,
  making it the natural second workflow feature after crash-resume.
