# Post-archive review: artifact-jobs

Date: 2026-09-17. Branch `artifact-jobs`, reviewed at `049ec2f` (base `9a5be05`).

Three reviewers ran a hard code review after the change was archived. The
verdict was **NOT OK**. Every finding was a defect measured against the
change's own specs or ADR-0006; none was new scope. This file records the
findings, which ones were reproduced, and how each was fixed. The archived
proposal, design, specs and tasks are left as they were. The living specs
under `openspec/specs/` and ADR-0006 were amended, and each amendment is
marked in place.

Every blocker was fixed test-first: a failing test was written, it was run
and failed for the stated reason, then the fix was applied.

## Commits

| Commit | Scope |
|---|---|
| `9a84168` | fix(workspace): never follow a symlink when writing an overlay |
| `d357967` | fix(claudecode): terminate the prompt and never lose a killed run's accounting |
| `aa930ba` | fix(engine): make permanent failures non-retryable and never retry an unmetered run |
| `850b9a2` | fix(ledger): key cost rows by execution so a re-run keeps its spend |
| `273fff9` | fix(gitcli): reuse only the root of a linked worktree, comparing resolved paths |
| `e9ea004` | docs: ADR-0006 amendments, living specs, DESIGN.md, ADR section citations in comments, this file |
| `f2d1a4d` | refactor(claudecode): unmetered error message names the context cause only when there is one |
| `759b8c1` | fix(engine): the model requirement moves behind the port; the deadline margin is derived from the runner's shutdown bound |

## Blockers

| # | Finding | Reproduced | Fix | Commit |
|---|---|---|---|---|
| B1 | A symlink tracked at the pinned commit *below* a root (`output/sub -> ../engine`) redirected overlay writes into engine code, and `Apply` returned nil. `os.Root` confines writes to the workspace, not to the roots. | Yes (reviewer and orchestrator). Adopted as `TestApply_SymlinkBelowRootInPinnedTree_EngineUntouchedAndRejected`: failed with a nil error before the fix. | The pre-check walks every source tree and Lstats every future destination component, including temp names. Writes open directories one component at a time with `O_NOFOLLOW` from a workspace descriptor. A symlink planted after the pre-check is refused (`TestApply_SymlinkDirPlantedAfterPrecheck_WriteRefusesToFollow`). | `9a84168` |
| B2 | A symlink at the temp name made `O_CREATE\|O_TRUNC` rewrite the engine file, and `Chmod` by name changed its mode. | Yes. Adopted as `TestApply_SymlinkAtTempName_EngineUntouchedAndRejected`, which also asserts the engine file's mode. | The pre-check rejects it. At write time the stale temp is unlinked, the new one is created with `O_CREAT\|O_EXCL\|O_NOFOLLOW`, and it is chmodded through the handle. A symlink planted after the pre-check is replaced, not followed (`TestApply_SymlinkTempPlantedAfterPrecheck_ReplacedNotFollowed`). | `9a84168` |
| B3 | A source holding both `.x.tollgate.tmp` and `x` silently lost a file. `.git` segments and non-regular files were not rejected everywhere, and sources were opened following symlinks. | Temp-name collision: red test failed (no error). `.git` in any case: red. FIFO: already rejected, now pinned by a test. | Entries ending in the temp suffix and `.git` segments in any letter case are rejected as `ErrUnsupportedSource`, in sources, and as `ErrOutsideRoots` in destinations. Sources are opened with `O_NOFOLLOW\|O_NONBLOCK` and must be regular. An unreadable source stays retryable. | `9a84168` |
| B4 | Directories created by the overlay were not durable: only the parents of renamed files were fsynced. | Red via an fsync observation seam: no created directory's parent was synced. | Every created directory's parent is scheduled for fsync, up to the first pre-existing directory (`TestApply_CreatedDirectories_ParentsFsynced`). | `9a84168` |
| B5 | `claude -p "--version"` printed the version. The prompt was a positional argument, so a dash-leading prompt parsed as a flag on both argv shapes. | Yes (reviewer and orchestrator). Red at the argv and runner level. | The prompt follows a `--` terminator after all flags, which also closes `--allowedTools`. Dash-leading prompts are not rejected. The PR-shape argv changed on purpose and is recorded in ADR-0006 §5. The byte-identical test now pins the safe argv. `requireSafeArgv` parses the argv with the CLI's grammar for table cases and `FuzzBuildArgs_NoPromptOrValueEverParsesAsAFlag` (4.9M execs, no failure). | `d357967` |
| B6 | A killed or timed-out run was classified "nothing billed" and retried. The 10-minute StartToClose timeout would SIGKILL a render. | Yes. The adapter test hung 30s: a killed run's stdout was held by its child, the B7 root cause. Engine red: an unmetered run was attempted 3 times under a 3-attempt policy. | Adapter: `ports.UnmeteredRunError` on ctx error or signal death with no envelope. Engine: non-retryable `AgentRunUnmetered`, logged with job_id and attempt, counted on `tollgate.agent.unmetered_runs`. `RunAgent` kills the agent a margin before the activity deadline, because a server-side timeout is retried regardless. Artifact jobs take `AgentTimeoutMinutes` (default 60, max 1440; ADR-0006 §10). `JobWorkflow` keeps 10 minutes. Comments claiming "nothing was billed" were corrected. | `d357967`, `aa930ba` |
| B6 follow-up | The orchestrator required that the deadline margin cover the whole shutdown path (group kill, `WaitDelay`, parse, return). With two independent constants it did not. | Yes. `TestActivities_RunAgent_RealRunnerShutdownFinishesBeforeActivityDeadline` spawns an escaped child that forces the full `WaitDelay`. With the old margin it returned after 6.003s of a 6s budget. | The margin is the runner's `ports.ShutdownBounder` bound (Claude Code: its `WaitDelay`) plus a 2s return slack. The residual case (worker crash or lost heartbeat, then a server-side retry whose spend stays unmetered) is stated in ADR-0006 Consequences. | `759b8c1` |
| B7 | No `WaitDelay` and no process-group kill: a background child holding stdout blocked `Output()` past a billed envelope. | Yes. The fake binary with `sleep 60 &` hung the test for 60s. | `Setpgid`, a `cmd.Cancel` that kills the group, and `WaitDelay` (10s). On `exec.ErrWaitDelay` the leftover group is killed and stdout is still parsed. The test also asserts the child is gone. | `d357967` |
| B8 | Resubmitting a job with the same JobID bills again, but `ON CONFLICT DO NOTHING` on `(job_id, phase, actor, attempt)` dropped the second row ($0.50 + $0.80 recorded as $0.50). | Yes, against Postgres (`TestLedger_SameJobIDTwoExecutions_BothRowsKept`: 1 row instead of 2). Also end to end on a real Temporal server and Postgres (`TestArtifactJobWorkflow_E2E_SameJobIDTwice_BothExecutionsLedgered`). Mutation-checked: with the run id blanked it records $0.50 instead of $1.30. | `run_id` (Temporal run id) is on every cost row of both shapes and in the natural key. Migration 00004 was amended in place, with a reversible Down that refuses to collapse executions. | `850b9a2` |
| B9 | Billed failures reported no cost and no model to telemetry, so the ledger and the OTel cost metric disagreed on this path. | Red at the telemetry and activity level. | `RunAgent` passes the billed result to `End`. `End` records usage, cost and `gen_ai.response.model` when the error wraps a `*ports.RunError`, and still marks the span as an error. | `aa930ba` |
| B10 | Sentinels documented as non-retryable reached Temporal as retryable plain errors: checkout, overlay and agent-config sentinels. | Yes. Through the Temporal test env, checkout and overlay sentinels were attempted 3 times and invalid configs 3 times. | `asNonRetryable` wraps each sentinel at the activity boundary with a typed name (ADR-0006 §10). Tests assert `NonRetryable()` and the attempt count through the Temporal test environment, using real activities with fake ports. | `aa930ba` |

## Warnings

| Warning | Resolution | Commit |
|---|---|---|
| `validate()`: absolute Repo, non-blank Prompt, no blank PieceID, parse the agent config, require a model | Repo, Prompt, PieceID and the timeout bounds are checked in `validate()`. **Deviation (approved by the orchestrator):** `validate()` runs in workflow code and cannot parse the adapter's format without breaking ADR-0002. A new first activity, `ValidateAgentConfig`, calls the optional port `ports.AgentConfigValidator` with `AgentConfigRequirements{RequireModel: true}`, and the adapter enforces the model rule, so the engine never parses agent settings. The failure is a non-retryable `InvalidAgentConfig`, attempted once before any checkout or overlay, and asserted through the Temporal test env (`TestArtifactJobWorkflow_InvalidAgentConfig_FailsBeforeCheckout`). | `aa930ba`, `759b8c1` |
| `null` / `{}` accepted as envelopes; noise lines | An envelope must have `type` `"result"` and `total_cost_usd`. The last JSON line is used when the whole output is not an envelope. | `d357967` |
| Agent config: trailing data, `tools: []`, separators and control characters | All rejected or distinguished as specified (ADR-0006 §5). | `d357967` |
| Billed path silently skipped the row on a decode failure; a RecordCosts failure hid the agent error | Both now fail with a non-retryable `AgentRunBilledUnrecorded` error, logged. **Deviation:** Temporal's failure converter follows a single cause chain; a multi-`%w` error reached the workflow result as an opaque `wrapErrors`. So the agent error is the cause and the ledger error is in the message. | `aa930ba` |
| Full Output in billed details | Dropped. Cost, usage and model are kept. | `aa930ba` |
| gitcli reuse: toplevel check, symlinks, `--` in `worktree add`, cleanup docs | Implemented. Reuse also refuses the repository's main worktree. Cleanup commands are in ADR-0006 Consequences. No dedicated test for `--`, because the workspace path is always absolute. | `273fff9`, `e9ea004` |
| File overlay onto a root; read-only leftover temp | Rejected in `Apply`'s pre-check. **Deviation:** `ValidatePaths` is pure and cannot tell a file source from a directory without a stat. The temp is created 0600 and the source mode is applied at the end. | `9a84168` |
| Bounded stderr tail in run errors | 2 KiB tail. | `d357967` |
| `JobWorkflow` activity-options test | `TestJobWorkflow_ActivityOptions_Unchanged` covers timeouts. `TestWorkflows_RetryCaps` checks retry caps by behavior, because the test env does not expose `ActivityInfo.RetryPolicy`. | `aa930ba` |
| Comments citing D1/D2/D4/D14 | Replaced with ADR-0006 section references (§1–§10). | `e9ea004` |

## Notes for operators

- A database that applied the *original* 00004 must be taken down with the
  original Down (drop `cost_entries_piece_id_idx` and `piece_id`) before the
  amended 00004 is applied. The branch was unmerged, so only test databases
  are affected.
- Re-running the same `job_id` requires removing its worktree first. The
  overlay leaves untracked files, so the strict reuse rule rightly reports a
  conflict.
- `openspec/changes/artifact-jobs/verify-report.md` still exists outside the
  archive. It predates this review and was left untouched.
