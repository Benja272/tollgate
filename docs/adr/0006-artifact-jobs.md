# ADR-0006: Artifact jobs — a second job shape with a frozen-engine boundary

Date: 2026-09-16
Status: proposed

## Context

Tollgate's only job shape ends in a PR (ADR-0001, ADR-0003). A downstream
content pipeline needs something else: run one headless agent over a
workspace pinned to a fixed commit, let it produce files, and measure what
that costs per model. One piece of content goes through three phases (plan,
preview, render), and a human approves each phase between jobs, outside
tollgate. The measurement is only clean if every billed token is operating
work. The agent must not be able to rebuild the engine it operates, and the
engine code must not change between the three jobs.

The existing code shapes the decision in four ways:

- `Prepare` never checks anything out. It only creates an empty directory
  (`internal/engine/activities.go:73-83`).
- `RunSpec` carries no model and no permission settings
  (`internal/ports/agent.go:8-11`). ADR-0002 says those settings live in "an
  opaque per-adapter config block, not as first-class fields".
- The `run_agent` ledger row has no model
  (`internal/engine/workflow.go:172-177`).
- When the CLI reports `is_error`, the runner discards the cost of a run that
  was already billed (`internal/adapters/claudecode/runner.go:65-67`).

## Decision

1. **A second workflow, `ArtifactJobWorkflow`.**
   - It validates its input, checks out the workspace, applies an overlay,
     runs the agent once, and records the cost.
   - It has no judges, no gate, no fix loop and no `Ship`.
   - `JobWorkflow` keeps its success-path command sequence. The only thing
     the two workflows share is a helper that runs the agent and records the
     `run_agent` row.
   - Two things do change for `JobWorkflow`, and both are data, not control
     flow. On the success path its `run_agent` rows now carry the model, and
     its `invoke_agent` spans gain `gen_ai.response.model`. On the error path
     it gains the billed-failure fix in §8.

2. **The source is pinned by commit SHA.**
   - `source_ref` must be exactly 40 hexadecimal characters, in either case.
     It is converted to lowercase before use, and surrounding whitespace is
     rejected rather than trimmed.
   - `repo` must be an absolute path to a local clone.
   - The checkout is a `git worktree add --detach` at that SHA. Every git
     invocation carries `-c core.hooksPath=/dev/null`, because
     `worktree add` fires the `post-checkout` hook of the clone it is run
     from.
   - Branches, tags and short SHAs fail validation before any activity runs.

3. **Overlay durability contract.**
   - The overlay is its own activity, separate from `RunAgent`, so a failed
     copy never bills the agent again.
   - Each file is written to a fixed temporary name in its destination
     directory, flushed to disk with fsync, and then renamed into place.
     Each directory the overlay touched is then fsynced as well.
   - Once the activity completes, every file is durable and complete. A
     retry rewrites everything and produces the same tree.

4. **The destination roots are the operator's frozen-engine boundary.**
   - The job input must declare at least one root, relative to the
     workspace. The overlay may write only inside those roots, and it may
     replace files that git tracks.
   - A root must be a non-empty local path. It cannot be `.`, and it cannot
     contain a `.git` component.
   - An overlay that leaves the roots fails non-retryably, and nothing is
     written. It can leave them lexically (through `..` or an absolute path)
     or physically (through a symlink on any existing component of a root or
     destination, or inside a source tree). Every overlay is checked before
     the first write.
   - As a second line of defense, writes go through `os.Root`, which keeps
     them inside the root directory.
   - Tollgate cannot tell which directories hold engine code. Declaring roots
     that leave the engine outside is the operator's responsibility.

5. **ADR-0002 is honored, not superseded.**
   - `RunSpec.AgentConfig` is an opaque JSON block. Only the adapter parses
     it, and an invalid block fails before any process is started.
   - For Claude Code the block is `{model, tools, allowed_tools}`. An
     artifact job must declare one.
   - A non-empty block runs the CLI with `--restricted`,
     `--strict-mcp-config`, `--permission-mode dontAsk` and
     `--permission-prompts none`, plus the model, the tool set and the
     allowlist.
   - An empty block produces exactly the command the adapter built before
     this change.
   - The adapter has no way to express `bypassPermissions` or
     `--dangerously-skip-permissions`, and `--restricted` refuses bypass
     anyway.
   - Verified against Claude Code 2.1.273 on 2026-09-16, with three real
     headless runs checked on disk:
     - A tool call outside the allowlist is denied without hanging. The run
       exits 0 with `is_error=false`, and the call is listed in
       `permission_denials`.
     - A `Bash(touch *)` rule allowed `touch` and still denied `mkdir`.
   - `--restricted` has three effects:
     - It ignores user, project and local settings files. A settings file at
       the pinned commit therefore cannot widen the allowlist.
     - It removes tools that run code unless `tools` names them.
     - It confines file tools to the working directory.

6. **What the allowlist bounds.** The allowlist bounds *mutating* commands.
   Commands the CLI classifies as read-only (for example `echo`) run whenever
   Bash is available, even with no Bash rule. `tools` decides whether Bash
   exists at all. An operator who wants no shell leaves Bash out of `tools`.

7. **Model identity is the resolved model id.**
   - The adapter reports `RunResult.Model` from the keys of the CLI's
     `modelUsage` block. The keys are resolved ids, for example
     `claude-haiku-4-5-20251001`.
   - Aliases such as `sonnet` change targets over time, so they cannot anchor
     a cost comparison.
   - When a run reports several models, the adapter picks the one with the
     highest reported cost. A tie goes to the lexicographically smallest id.
   - It falls back to the requested alias only when the CLI reports no
     per-model usage.
   - When a run reports no per-model usage and no alias was requested (the
     empty-config PR shape), the model is recorded as the constant
     `unknown`, and a warning is logged. A billed run must never fail over a
     missing model label, and an empty label would break the rule that every
     `run_agent` row records a model.
   - Reporting the model back is output normalization, which ADR-0002
     already allows: the interface exists to normalize what each agent
     reports into the ledger's `CostEntry` rather than leak vendor formats
     upward (`0002:33-34`), and telemetry is explicitly what the agent
     reports plus what tollgate measures around it (`0002:39-41`). The model
     stays out of `RunSpec` as a first-class input; only the result carries
     it.
   - The model is written to the `run_agent` ledger row and to
     `gen_ai.response.model`, for both job shapes.

8. **A billed failure is still recorded, in both job shapes.**
   - When the CLI prints a parseable result envelope and the run failed
     (`is_error=true` or a non-zero exit), the runner returns a typed error
     that carries the partial result: cost, usage and model.
   - The engine turns it into a non-retryable `AgentRunBilled` error with that
     result attached. The shared helper records the `run_agent` row, and only
     then does the job fail.
   - With no parseable envelope (the process crashed or was killed), there is
     no reported cost to record, and the failure stays retryable as before.
   - This deliberately fixes the PR shape's cost accounting on its error
     path. Cost accounting is a blocking review axis, and before this change a
     billed error run was dropped from the ledger and then silently retried,
     billing twice.
   - On the PR shape's success path the only change is the model recorded on
     the row and on the span (§1, §7). No step is added, removed or
     reordered.
   - The success-path histories (ship and reject) are unaffected, because
     they contain no failed agent runs.

9. **`piece_id` is a nullable ledger column.**
   - It is not part of the natural key `(job_id, phase, actor, attempt)`.
   - The cost of a piece is the sum over all of its jobs, whatever each job's
     outcome.
   - Knowing which pieces completed is the operator's concern, so the ledger
     has no job-status column.

## Rationale

- **SHA, not branch.** Parallel sessions pushed three engine commits to the
  consumer repo in seven days. A branch resolved separately by each of a
  piece's jobs can drift between them. Requiring a SHA prevents drift, where
  recording the resolved SHA would only detect it afterwards.
- **A local clone, not a URL.** `git worktree add` needs a local repository
  anyway, and worktrees share its object store. The worker needs no network
  credentials and no fetch policy.
- **Enforced isolation, not a documented convention.** Relying on the
  operator to keep the pinned repo's settings files narrow would leave the
  allowlist only as strong as a convention. `--restricted` makes the job's
  config the only source of permissions, apart from managed settings.
- **A non-retryable billed failure.** A retried activity surfaces only its
  last error. If a billed failure were retried, the spend of the earlier
  attempt would be lost, and the retry would be a paid call nobody decided to
  make.
- **A separate, thin workflow.** An artifact job has no loop to share, and
  branching inside `JobWorkflow` would put its replay history at risk.

## Alternatives considered

- **First-class `Model` and allowlist fields on `RunSpec`.** Rejected: they
  contradict ADR-0002, and a second adapter would inherit fields that mean
  nothing to it.
- **The `default` permission mode.** Rejected: the CLI no longer offers it.
  `dontAsk` together with `--permission-prompts none` is the verified way to
  deny without hanging.
- **Recording the requested alias.** Rejected: it stays stable in the ledger
  but becomes wrong once the alias moves.
- **Returning success with a failure flag on a billed error.** Rejected: it
  hides a failed run behind a completed activity.
- **Hardlink or reflink overlays.** Rejected for now: they depend on the
  filesystem, and a hardlink shares a writable inode. They may return later
  as an optimization.
- **Encoding the piece in the job id.** Rejected: string parsing that nothing
  enforces. It remains the fallback if the migration must be cut.
- **Rejecting overlays onto tracked files.** Rejected: phase N+1 must receive
  phase N's edited versions of tracked files.

## Consequences

- **No automatic workspace cleanup.** The workspace is how artifacts are
  handed off, so disk use grows until an operator runs
  `git worktree remove`.
- **Strict checkout reuse.** An existing checkout is reused only if it
  shares the object store, `HEAD` is at the SHA, and `git status` is clean.
  Any other existing path is a non-retryable conflict. A checkout that
  crashed halfway must be removed by hand.
- **Replay-tested history.** The `JobWorkflow` history is replay-tested
  against histories captured before this change.
- **PR-shape error path.** A PR-shape job whose agent run fails with a billed
  envelope is no longer retried automatically, and its spend now appears in
  the ledger.
- **Remaining unrecorded spend.** A run killed before it printed an envelope
  is still unmeasured.
