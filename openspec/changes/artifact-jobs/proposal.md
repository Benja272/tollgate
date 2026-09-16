# Proposal: Artifact jobs — a bounded, no-PR job shape with a clean cost boundary

## Intent

A downstream content pipeline (a video-editing engine driven by a Claude Code agent) must compare the cost per piece of Sonnet 5 and Opus 5 to decide whether its pricing is viable. Transcripts cannot separate operating the engine from building it. Tollgate can: run one headless agent job over a workspace pinned to a fixed commit, so every billed token is operating work by construction. The three phases of a piece (plan, preview, render) run as three jobs. Humans approve each phase between jobs, outside tollgate.

## Scope

### In Scope
- `ArtifactJobWorkflow`: validate input, check out, overlay, run the agent once, record costs. It runs no judges and no gate and opens no PR.
- A real git-worktree checkout of `Repo` at `SourceRef`, detached. This step belongs to artifact jobs only.
- A generic overlay activity. It places files from host paths into the workspace after checkout, and it is safe to retry.
- An agent config block with a model and a tool allowlist. The Claude Code adapter turns it into flags and never uses a bypass or skip-permissions mode.
- `run_agent` cost rows record the model.
- A nullable `piece_id` column on the ledger, outside the natural key.
- ADR-0006.

### Out of Scope (non-goals)
- A submit API or submit CLI. Jobs start through `temporal workflow start`.
- Human signals and pausing.
- Artifact rubrics and judges.
- Reflink or hardlink optimizations for the overlay.
- Any marketing-specific code. Destination roots are job input, never hardcoded.
- A job-status column in the ledger.
- Workspace cleanup. **Consequence:** the workspace is how artifacts get handed off, so it is kept on purpose. Disk use grows until an operator runs `git worktree remove`.
- Real checkout for the PR-shaped `Prepare`. It stays untouched.

## Resolved Decisions (with evidence)

| # | Decision | Evidence |
|---|---|---|
| R1 | `SourceRef` must be a full 40-character hex commit SHA. Branches, tags and short SHAs fail input validation as non-retryable errors. | The consumer repo received 3 engine commits in 7 days from parallel sessions. A branch resolved separately by each of a piece's three jobs can drift and silently contaminate the measurement. Requiring the SHA prevents drift, where recording the resolved hash would only detect it afterwards. The operator's extra step is one `git rev-parse HEAD`. |
| R2 | Overlays onto files git tracks are **allowed**. The job input declares one or more destination roots, relative to the workspace. After path cleaning, every overlay destination must fall inside one of them. Symlink escapes are rejected, and anything outside the roots fails non-retryably. | The consumer repo tracks 319 files under its job directories. Phase N+1 must receive phase N's edited versions of exactly those files. The engine code lives outside the declared roots, so it stays frozen. |
| R3 | Every attempt's spend counts toward the piece, including jobs that failed after the agent ran. The per-piece total is all spend across the piece's jobs, whatever their outcome. | Which pieces completed is the operator's knowledge, so no status column is needed. |
| R4 | Checkout is a step for artifact jobs only. The PR-shaped `Prepare` stays untouched. | In the PR shape, `Repo` is a GitHub slug and `SourceRef` is an issue id (`workflow_test.go:73`). |

## Capabilities

### New Capabilities
- `artifact-job`: the no-PR job shape, its input and its validation (R1), its result, and how it ends.
- `workspace-preparation`: the pinned worktree checkout, and the overlay's durability and destination-root contract (R2).
- `agent-run-config`: the per-adapter model and allowlist, never a bypass.
- `cost-ledger`: the model on `run_agent` rows and cost aggregation per piece (R3).

### Modified Capabilities
- None. `openspec/specs/` is empty.

## Approach

- **ADR-0002 is honored, not superseded.**
  - `RunSpec` gains an opaque `AgentConfig json.RawMessage`. Only the adapter parses it, and an invalid config fails before the process is spawned.
  - The adapter reports the model back in `RunResult.Model`. This is output normalization, which the ADR already allows.
- **Input validation is pure code at the start of the workflow.** It checks the SHA format and the lexical containment of overlay destinations inside the declared roots. Symlink resolution needs the filesystem, so the overlay activity re-checks it there.
- **Workflow split.** A second, thin workflow. The fix-loop extraction was dropped because an artifact job has no loop to share. Only "run the agent, then record the `run_agent` cost" is the same logic in both workflows, so that becomes one shared helper.
- **Overlay durability.**
  - The overlay is its own activity, so a failed copy never re-bills the agent.
  - Each file is copied to a temp name, then renamed into place, so a retry overwrites it atomically.
- **Checkout retry.** An existing worktree at the same SHA is reused. Any other existing path is a non-retryable error.
- **ADR-0006** records:
  - the new job shape;
  - the SHA-only rule (R1);
  - the overlay durability and destination-root contract (R2);
  - the opaque config with the model reported back;
  - `piece_id` (R3).
- **Handed to design:** choose between recording the requested model alias and the resolved model id from the CLI output. Aliases change their target over time.

## Affected Areas and Line Estimate (production)

| Area | Impact | ~Lines |
|---|---|---|
| `internal/ports/{agent,ledger}.go` | Modified | 15 |
| `internal/adapters/claudecode/runner.go` | Modified | 70 |
| Checkout (git adapter behind a port) | New | 80 |
| Input validation (SHA, destination roots) | New | 30 |
| Overlay activity (roots, symlink check, rename) | New | 110 |
| `internal/engine/` workflow, types, shared helper | New/Modified | 110 |
| `internal/adapters/postgres` + migration | Modified/New | 30 |
| `cmd/worker/main.go` | Modified | 5 |
| `docs/adr/0006-*.md`, `docs/DESIGN.md` | New/Modified | 100 |

Total is about 550 lines, which fits the 800-line budget in a single PR. If it runs over, cut in this order:
1. Replace the `piece_id` migration with a JobID naming convention.
2. Drop the model from the telemetry span.

## Verification owed

- Strict TDD: every task starts with a failing test.
- The PR shape is proven unchanged three ways:
  - `workflow_test.go` and `crash_resume_test.go` pass without any edited assertions.
  - A `WorkflowReplayer` test replays a `JobWorkflow` history captured before the change.
  - With an empty config, the adapter builds exactly today's command-line arguments.
- One end-to-end test on a real Temporal dev server (the dev-strategy scar log asks for this). It checks that an artifact job records `run_agent` rows with the model and never calls `Ship`.
- The overlay produces the same result when its activity is retried after a failure partway through.
- Rejection tests, each asserting a non-retryable error:
  - an unsafe ref: a branch, a tag, or a short SHA;
  - an overlay destination that escapes the roots through `..`;
  - an overlay destination that escapes the roots through a symlink.

## Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| Destination roots are declared too broadly (for example, `.`) and expose the engine to the overlay | Med | ADR-0006 documents that the roots are the operator's frozen-engine boundary |
| An allowlist mistake stalls or denies the headless run | Med | Adapter tests on the flags, plus the end-to-end test |
| A model alias hides which model actually ran | Med | Design decides between the alias and the resolved model id |
| Large overlays are slow to retry | Med | Rename-into-place. Optimizations stay non-goals |

## Rollback Plan

Revert the PR. The migration only adds a nullable column and has a goose down step. The new workflow is additive, so unregistering it leaves the PR shape untouched.

## Success Criteria

- [ ] An artifact job started from the CLI with a full SHA produces artifacts in a worktree at that commit, with no PR. A branch ref is rejected before any paid call.
- [ ] Phase N's edited tracked files overlay into phase N+1 inside the declared roots. Writes outside the roots fail.
- [ ] Ledger rows for one piece's three jobs sum by `piece_id` regardless of job outcome, and each shows the model.
- [ ] The replay test and the existing tests pass unchanged.
