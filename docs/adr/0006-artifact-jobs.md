# ADR-0006: Artifact jobs — a second job shape with a frozen-engine boundary

Date: 2026-09-16
Status: proposed
Amended: 2026-09-17, after the post-archive review
(`openspec/changes/archive/2026-09-16-artifact-jobs/post-archive-review.md`).
Amended text is marked *(amended)*. The second review round (same file,
"Second review round") amended it again; that text is marked
*(amended, round 2)*.

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
   - *(amended)* The temporary file is created writable (0600) with
     `O_CREAT|O_EXCL|O_NOFOLLOW`, after any leftover at that name is
     removed. It gets the source's mode through its open handle just before
     the fsync. A leftover read-only temp file therefore cannot block a
     retry, and a symlink at the temporary name is replaced, never followed.
   - *(amended, round 2)* Every directory on the path to a destination is
     durable: the parent of every path component is fsynced, whether this
     call created the component or an earlier, failed attempt did. The
     first amendment queued only directories the current call created, so
     a retry never made the failed attempt's directories durable. That was
     reproduced.
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
   - *(amended)* The check covers every future destination component of
     every file in every source tree, including each file's temporary name.
     The first version checked only each overlay's `Dest`, so a symlink
     tracked at the pinned commit *below* a root (`output/sub ->
     ../engine`) redirected writes into engine code. That was reproduced.
   - *(amended)* The second line of defense is no longer `os.Root`, which
     confines writes to the workspace, not to the roots. Every write opens
     its directories one component at a time from a workspace descriptor
     with `O_NOFOLLOW` (`openat`, `mkdirat`, `renameat`). A symlink planted
     after the check is refused (`ErrOutsideRoots`), never followed. This
     makes the overlay Unix-only, as the adapter already is.
   - *(amended)* Also rejected before any write:
     - source entries that are not regular files or directories;
     - source entries whose name ends in the temporary-file suffix, which
       could otherwise collide with another file's temporary name;
     - a `.git` segment in any letter case, in a source tree or in a
       destination (a planted `.git/config` with `core.fsmonitor` is a code
       execution vector);
     - a file overlay whose `Dest` equals a root.

     Sources are opened with `O_NOFOLLOW|O_NONBLOCK` and must be regular
     files, so a source swapped for a symlink or FIFO after the check is
     refused rather than followed or blocked on. Unreadable sources stay
     retryable.
   - *(amended, round 2)* A destination that can never be written is a
     non-retryable `ErrDestinationConflict` (`OverlayDestinationConflict`),
     found by the pre-check and, if the workspace changes shape afterwards,
     by the write path (`ENOTDIR`, `EISDIR`, `ENAMETOOLONG`, `EEXIST`,
     `ENOTEMPTY`). The cases are:
     - a file overlay onto a directory;
     - a path through a file;
     - a file and a directory overlay at one path, or a path below a file
       overlay;
     - a name too long for its temporary name (255 bytes);
     - a directory at a temporary name.
   - *(amended, round 2)* The temporary-file suffix is reserved for the
     whole workspace. The checkout refuses a pinned tree that tracks any
     path with a segment ending in it (`ErrReservedPath`,
     `ReservedPathInTree`), so a temp-suffixed file found in a workspace is
     always a leftover of ours and safe to remove. Without this, a tracked
     `.f.tollgate.tmp` was silently deleted when the overlay wrote `f`. That
     was reproduced.
   - *(amended, round 2)* Writing is behind a `unix` build tag. Elsewhere
     the module builds and `Apply` fails with `errors.ErrUnsupported`
     (`UnsupportedPlatform`).
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
   - *(amended)* The prompt is always the single operand after a `--`
     terminator placed after every flag:
     `claude -p --output-format json [flags] -- <prompt>`. The CLI takes the
     prompt as a positional argument, so before this amendment a prompt
     starting with `-` parsed as an option on both argv shapes.
     `claude -p "--version"` printed the version, and a prompt of
     `--dangerously-skip-permissions` would have passed the bypass flag.
     Dash-leading prompts are legitimate (markdown lists) and are not
     rejected. Verified on Claude Code 2.1.274:
     `claude -p --model haiku --output-format json --allowedTools "Read" --
     "--version is not a flag here. Reply PONG."` returns PONG. The
     terminator therefore also closes the variadic `--allowedTools`.
   - *(amended)* An empty block produces the minimal command
     `-p --output-format json -- <prompt>`: the same flags as before this
     change, with the prompt moved behind the terminator. This deliberately
     changes the PR shape's argv, for security.
   - *(amended)* Config values are validated and never rewritten. The
     adapter rejects:
     - trailing data after the JSON object;
     - `tools` entries containing a comma, whitespace or a control
       character (entries are joined with commas);
     - `allowed_tools` entries with a control character, unbalanced
       parentheses, or a comma or whitespace outside parentheses (the CLI
       splits rule lists on those).

     `"tools": []` emits `--tools ""` (no tools). An absent key emits
     nothing (the CLI default).
   - *(amended)* A new activity, `ValidateAgentConfig`, is the first step of
     an artifact job. Workflow code cannot parse the block, because the
     format belongs to the adapter (ADR-0002). The engine never parses agent
     settings; it only states what the job shape requires.
   - *(amended)* The activity calls the optional port
     `ports.AgentConfigValidator` with
     `AgentConfigRequirements{RequireModel: true}`. The adapter parses the
     block exactly as `Run` would and enforces the requirement in its own
     terms. An artifact job measures cost per model, so it must never run
     on the harness default.
   - *(amended)* A failure is a non-retryable `InvalidAgentConfig` error,
     attempted once before any checkout or overlay. Tests assert this
     through the Temporal test environment. A runner that cannot validate
     without running is trusted at this step and is checked again by
     `RunAgent`.
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
   - *(amended)* With no parseable envelope there is no reported cost to
     record, and nothing is written. That does not mean nothing was billed.
     A run killed by its deadline, a cancellation or a signal before
     printing an envelope returns `ports.UnmeteredRunError`. The engine
     turns it into a non-retryable `AgentRunUnmetered` error, logs it with
     `job_id` and attempt, and counts it on `tollgate.agent.unmetered_runs`.
     A retry would bill again on top of an unknown amount. Only a run that
     exited on its own without an envelope stays retryable, within the
     attempt cap.
   - *(amended)* A StartToClose timeout is decided by the server, which
     retries whatever the activity returns afterwards. `RunAgent` therefore
     stops the agent a margin before the activity deadline, so the
     unmetered error reaches Temporal while the attempt is still live.
   - *(amended)* The margin is derived, not a second independent constant:
     - It is the runner's shutdown bound (`ports.ShutdownBounder`) plus a
       2-second return slack. The slack covers parsing the output, mapping
       the error, ending the span and reporting the result.
     - For Claude Code the bound is the runner's `WaitDelay`, because the
       group kill is immediate. That makes the default margin 12 seconds.
     - A runner that states no bound gets 30 seconds.
     - If the activity budget is smaller than the margin, the run is
       refused rather than started without time to shut down. *(amended,
       round 2)* The refusal has its own non-retryable type,
       `AgentRunBudgetTooShort`, and is not counted as unmetered, because
       nothing ran.
     - A real-runner test pins the invariant with an escaped child
       process: it forces the full `WaitDelay`, and the activity must still
       return before its deadline.
   - *(amended)* The CLI runs in its own process group. Cancellation kills
     the whole group, and `WaitDelay` (10 seconds) bounds the wait for
     output pipes: a background process started by the agent's Bash tool
     inherits stdout. Before this amendment, such a process held the runner
     open past a printed, billed envelope until the activity timed out.
   - *(amended, round 2)* The group is killed on **every** return from
     `Run`, not only on timeout. Otherwise a watcher or dev server left by
     the agent kept mutating the workspace after the run; that was
     reproduced.
     - On Linux the kill happens after the group leader exits but before it
       is reaped (`waitid(WNOWAIT)`). A zombie leader keeps its pid, and so
       the group id, from being reused, so the kill can only reach
       processes the agent started.
     - On other Unix systems the leader is already reaped when the group is
       killed. The group id could in principle have been reused (see
       Consequences).
     - On Linux the CLI also gets `Pdeathsig=SIGKILL`, with its starting OS
       thread locked until it is reaped, so it dies with the worker instead
       of racing the server's retry in the same workspace.
     - A process that escaped the group (`setsid`) can still hold stdout.
       `WaitDelay` bounds that wait, and the captured stdout is still read.
   - *(amended, round 2)* The failure classes, with no usable envelope:
     - exit 0 (a clean exit without a cost report): **unmetered**;
     - death by signal: **unmetered**;
     - an exit status above 128: **unmetered**. Claude Code 2.1.274 traps
       SIGTERM and SIGHUP and exits 143 or 129, which a systemd stop
       triggers. That was reproduced;
     - a context that ended while the run was live: **unmetered**;
     - an exit of 1..128 on its own: plain and retryable;
     - a CLI that never started (missing binary, context already done,
       unsupported platform): plain and retryable, since nothing ran.
   - *(amended, round 2)* stdout must hold **exactly one** result envelope
     (a JSON object with `type` `"result"`): either the whole output, or a
     single line among noise lines.
     - Anything that inherited stdout can print a line. The first amendment
       took the last envelope line, and a forged cost-0 line printed after
       the real one was recorded as the run's cost; that was reproduced.
     - More than one envelope is `ports.ErrAmbiguousEnvelope`, handled like
       an unmetered run: logged, counted, non-retryable, with type
       `AgentRunAmbiguousEnvelope`.
     - One envelope without a numeric `total_cost_usd` is not usable.
   - *(amended, round 2)* Captured stdout is capped at 64 MiB. Output past
     the cap means something other than the CLI is writing, so the run is
     unmetered.
   - Run errors carry a bounded (2 KiB) stderr tail.
   - *(amended)* The billed error's details carry cost, usage and model,
     but not the agent's output, which could exceed Temporal's payload
     limit. The billed span records the same cost, usage and
     `gen_ai.response.model` as the ledger row, and is still marked as an
     error.
   - *(amended)* If the details cannot be decoded, or the row cannot be
     written, the job fails with a non-retryable `AgentRunBilledUnrecorded`
     error. It is logged, its message says the spend was not recorded, and
     the agent's error is its cause.
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
   - It is not part of the natural key.
   - *(amended)* The natural key is `(job_id, run_id, phase, actor,
     attempt)`. `run_id` is the Temporal run id of the execution, recorded
     on every cost row of both job shapes. Re-running a piece under the same
     `job_id`, for example to compare models, bills again. With the old key,
     `ON CONFLICT DO NOTHING` dropped the second execution's row ($0.50 +
     $0.80 was recorded as $0.50). That was reproduced against Postgres.
     Retries within one execution still collide and stay idempotent.
     `run_id` is `NOT NULL DEFAULT ''`, because a NULL would never conflict.
     Migration 00004 was amended in place, since the branch was unmerged.
     Its Down refuses to collapse executions rather than delete spend.
   - *(amended, round 2)* `run_id` is the run that **paid**, not the run
     that writes the row.
     - A workflow reset between a paid activity and `RecordCosts` makes the
       new run re-issue the write. Keyed by the writing run, one billed run
       became two rows ($0.50 recorded as $1.00, and the per-piece total
       inflated the same way). That was reproduced against a real server
       and Postgres.
     - `RunAgent` and `JudgeOne` return
       `activity.GetInfo(ctx).WorkflowExecution.RunID` as `PaidByRunID`, in
       the result and in the billed-failure details. Rows use it, and fall
       back to the current run only for results journaled before the field
       existed.
     - A reset *before* the paid activity re-runs the call. That is a second
       real charge, and it correctly gets a row under the new run.
     - The ledger rejects a new row with an empty `run_id`
       (`ErrMissingRunID`); only legacy rows carry `''`.
     - Real-server reset tests cover an artifact job reset after `RunAgent`,
       and a `JobWorkflow` reset after `RunAgent` and after `JudgeOne`.
   - The cost of a piece is the sum over all of its jobs, whatever each job's
     outcome.
   - Knowing which pieces completed is the operator's concern, so the ledger
     has no job-status column.

10. **Failure classification at the activity boundary** *(added)*.
    A plain Go error reaches Temporal as retryable, whatever its
    documentation says. Every permanent port sentinel is wrapped as a typed,
    non-retryable application error, with the sentinel kept as its cause:

    | Activity | Sentinel | Type |
    |---|---|---|
    | `CheckoutWorkspace` | `ErrCheckoutConflict`, `ErrInvalidRepo`, `ErrRefNotFound` | `CheckoutConflict`, `InvalidRepo`, `RefNotFound` |
    | `ApplyOverlay` | `ErrOutsideRoots`, `ErrUnsupportedSource` | `OverlayOutsideRoots`, `OverlayUnsupportedSource` |
    | `ValidateAgentConfig`, `RunAgent` | `ErrInvalidAgentConfig` | `InvalidAgentConfig` |
    | `RunAgent` | `*RunError` / `*UnmeteredRunError` | `AgentRunBilled` / `AgentRunUnmetered` |
    | `CheckoutWorkspace` *(round 2)* | `ErrReservedPath` | `ReservedPathInTree` |
    | `ApplyOverlay` *(round 2)* | `ErrDestinationConflict` | `OverlayDestinationConflict` |
    | `RunAgent` *(round 2)* | `*UnmeteredRunError` wrapping `ErrAmbiguousEnvelope` | `AgentRunAmbiguousEnvelope` |
    | `RunAgent` *(round 2)* | budget below the kill margin (engine) | `AgentRunBudgetTooShort` |
    | `RunAgent`, `ApplyOverlay` *(round 2)* | `errors.ErrUnsupported` | `UnsupportedPlatform` |
    | workflow helper | a billed row that could not be written | `AgentRunBilledUnrecorded` |

    *(amended, round 2)* Checkout wraps a git failure in its sentinel only
    when the failure is permanent. A done context (worker shutdown, attempt
    timeout) or a missing `git` binary stays a plain, retryable error;
    before this amendment, a shutdown failed the job forever as
    `InvalidRepo`.

    Tests assert the non-retryable flag and a single attempt through the
    Temporal test environment.

    **The artifact agent timeout is job input.** `AgentTimeoutMinutes`
    ranges from 0 to 1440, and 0 means 60 minutes. The render phase drives
    ffmpeg over footage and routinely exceeds the ten minutes a PR-shape
    agent run gets. A run cut off by the timeout is unmetered and never
    retried, so a default that is too short turns into lost, unrecorded
    spend. An hour covers a render with margin and still bounds a hung run.
    *(amended, round 2)* The bounds are checked on the integer minutes
    before multiplying. `time.Duration(m) * time.Minute` wraps: `1<<53`
    passed as 0 (the default) and `1<<53+1` as one minute, which would kill
    a render.

    **No `workflow.GetVersion` for the new first activity** *(round 2)*.
    Adding `ValidateAgentConfig` before `CheckoutWorkspace` changes the
    command sequence. That is safe only because no worker ever ran
    `ArtifactJobWorkflow` before this branch merged, so no history exists
    to break. Replay fixtures for the success and billed-failure paths were
    captured at the end of the review. From now on, a change that fails
    `TestArtifactJobWorkflow_ReplaysCapturedHistories` needs
    `workflow.GetVersion`, not a new capture.

    **Worker startup** *(round 2)*. The worker requires an absolute
    workspace root (`TOLLGATE_WORKSPACE_ROOT`, default: the OS temp
    directory). The checkout also refuses a relative workspace path, and
    `worktree add` receives the path after `--`.
    `JobWorkflow` keeps its 10-minute agent timeout. Its activity options
    are pinned by a test, because the replay test cannot see them.

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
  handed off, so disk use grows until an operator cleans up by hand. From
  the source clone, run
  `git -C <repo> worktree remove --force <WorkspaceRoot>/tollgate-artifact-<job_id>`.
  If the directory was already deleted, run `git -C <repo> worktree prune`
  to drop the stale administrative entry. A job overlay leaves untracked
  files, so re-running the same `job_id` requires removing its worktree
  first (see the next item).
- **Strict checkout reuse.** An existing checkout is reused only if it
  shares the object store, `HEAD` is at the SHA, and `git status` is clean.
  *(amended, round 2)* "Clean" means all of the following:
  - `git status --porcelain --ignored --untracked-files=all` is empty;
  - `git ls-files -v` shows no assume-unchanged (lowercase tag) or
    skip-worktree (`S`) entry.

  Plain `git status` hides ignored files, and hides edits behind those index
  flags. An edited, hidden engine file was reused as "clean at the SHA";
  that was reproduced. The workspace path must not itself be a symlink: a
  link to another job's worktree at the same SHA passed every other check.
  *(amended)* It must also be the root of a *linked* worktree: its resolved
  path must equal `rev-parse --show-toplevel`, and its git dir must differ
  from the common dir. Common dirs are compared after resolving symlinks.
  `worktree add` receives its path after `--`.
  Any other existing path is a non-retryable conflict. A checkout that
  crashed halfway must be removed by hand.
- **Replay-tested history.** The `JobWorkflow` history is replay-tested
  against histories captured before this change.
- **PR-shape error path.** A PR-shape job whose agent run fails with a billed
  envelope is no longer retried automatically, and its spend now appears in
  the ledger.
- **Remaining unrecorded spend.** A run killed before it printed an envelope
  is still unmeasured. *(amended)* It is no longer silent: it is logged,
  counted, and never retried.
- **Residual case the margin cannot cover** *(added)*. Sometimes the worker
  cannot return anything:
  - the worker process crashes;
  - the host dies;
  - heartbeats are lost long enough for `HeartbeatTimeout` to expire.

  Then the server times the attempt out and retries it under the activity's
  retry policy. That attempt's spend stays unmetered: nothing records it,
  nothing counts it, and the retry can bill again. The bound is the retry
  cap, `runAgentMaxAttempts` (2). Closing this gap needs a record written
  before the agent starts (an intent row reconciled after the fact), which
  is out of scope here. On Linux, `Pdeathsig` at least stops the orphaned
  CLI from racing that retry in the same workspace.
- **Group-kill residual outside Linux** *(round 2)*. Without
  `waitid(WNOWAIT)` the leader is reaped before its group is killed, so
  that kill could in principle reach a new process group that reused the
  id. On Linux the kill happens while the leader is an unreaped zombie, so
  the id cannot be reused.
- **Migration 00004 locks the table while it rebuilds the natural key**
  *(round 2)*. `CREATE UNIQUE INDEX` without `CONCURRENTLY` takes an
  exclusive lock for the duration of the build. That is fine at the
  ledger's current size. A large ledger would need
  `CREATE UNIQUE INDEX CONCURRENTLY` in its own non-transactional
  migration.
- **Judge calls are not hardened (follow-up)** *(round 2)*. These defects
  predate this change and affect the PR shape only, since artifact jobs run
  no judges:
  - `claudecode.CLIJudge` drops billed failures and lets them retry: an
    envelope with `is_error`, an invalid verdict JSON after a billed call,
    or a non-zero exit after an envelope.
  - It runs with no process group, no `WaitDelay`, no working directory,
    and the CLI's default tools.

  Its paid-run id is in scope and fixed (§9). The rest is recorded as a
  follow-up in the post-archive review.
