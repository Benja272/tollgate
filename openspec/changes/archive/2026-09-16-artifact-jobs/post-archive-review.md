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

## Second review round

Date: 2026-09-17. A three-way re-review of `049ec2f..fabf977` returned
**NOT OK**. Most findings were reproduced by the reviewers, either against
Claude Code 2.1.274 with a fake local API or against a real Temporal server
and Postgres. The fixes were made test-first again. Reviewed at `abf22b3`,
where the only change was the orchestrator removing the stray pre-archive
verify report.

### Commits

| Commit | Scope |
|---|---|
| `740fb31` | fix(ledger): key a cost row by the run that paid, not the run that writes it |
| `72fee13` | fix(engine): bound the agent timeout before multiplying and refuse unrunnable budgets separately |
| `d054980` | fix(claudecode): treat every run without one trustworthy cost report as unmetered |
| `efc5f43` | fix(workspace): make retried directories durable and classify destination conflicts |
| `8b2eacb` | fix(gitcli): reuse only an untouched worktree and keep transient failures retryable |
| `7ea599b` | test(engine): capture ArtifactJobWorkflow replay fixtures as its determinism gate |
| `c9d24b3` | ADR-0006 round-2 amendments, living specs, CLI doc, this section |

### Blockers

| # | Finding | Red test (failed before the fix) | Fix | Commit |
|---|---|---|---|---|
| R1 | A workflow reset between a paid activity and `RecordCosts` wrote the billed run again under the new run id: 1 run became 2 rows, $0.50 was recorded as $1.00, and the per-piece total inflated too. | `TestArtifactJobWorkflow_E2E_ResetAfterRunAgent_NoDoubleCount` (2 rows) and `TestJobWorkflow_E2E_ResetAfterRunAgentAndAfterJudge_NoDoubleCount` (4 and 3 rows), both on a real server with Postgres. Also the unit tests `TestRunAgent_ResultAndBilledDetailsCarryThePayingRunID`, `TestRecordCosts_UsesThePayingRunID_FallsBackToTheWritingRun` and `TestJudgeOne_JudgmentCarriesThePayingRunID`. | `RunAgent` and `JudgeOne` return `PaidByRunID`, in the result and in the billed details, and rows use it; the current run is only a fallback for legacy results. A reset after `RunAgent` in `JobWorkflow` correctly re-runs the judge, a second real charge, so that case expects 3 rows. | `740fb31` |
| R2 | `time.Duration(AgentTimeoutMinutes) * time.Minute` wrapped before the bounds check (`1<<53` passed as 0, `1<<53+1` as 1 minute). | `TestArtifactJobWorkflow_InvalidInput_ReviewAdditions` (overflow cases passed validation); `TestArtifactJobWorkflow_AgentTimeout_UpperBoundAccepted` pins 1440. | Bounds are checked on the integer minutes. | `72fee13` |
| R3 | Claude Code traps SIGTERM and SIGHUP and exits 143 or 129 without an envelope, which was classified as retryable. | `TestRunner_Run_ExitAbove128WithoutEnvelope_IsUnmetered` (129, 143, 255; 128 stays plain). | An exit above 128 without an envelope is unmetered. | `d054980` |
| R4 | Exit 0 without a usable envelope was a retryable parse error; the old test locked that in. | `TestRunner_Run_NoUsableEnvelope_ExitZeroIsUnmeteredExitOneIsPlain` replaces `TestRunner_Run_NotAnEnvelope_Rejected`. | A clean exit without a usable envelope is unmetered. | `d054980` |
| R5 | A forged result line after the real envelope was recorded, because the last envelope line won. That rule came from the orchestrator's first brief, and no test pinned the choice. | `TestRunner_Run_EnvelopeCount`: one, forged-after, forged-before, forged by an escaped child, and a cost-less result line next to a real one. | Exactly one result envelope is required. More than one is `ErrAmbiguousEnvelope`, logged and counted like unmetered, with type `AgentRunAmbiguousEnvelope`. | `d054980` |
| R6 | Processes left by the agent survived `Run` unless it timed out. | `TestRunner_Run_LeftoverProcessesKilledOnEveryReturn` (non-zero exit with an envelope, clean exit, exit without an envelope); `TestConfigureProcess_SetsGroupAndParentDeathSignal`. Mutation-checked: removing the group kill fails both group-kill tests. | The group is killed on every return. On Linux this happens after the leader exits and before it is reaped (`waitid(WNOWAIT)`), so the group id cannot be reused, and `Pdeathsig=SIGKILL` is set with the starting thread locked. The reap-before-kill residual on other Unix systems is recorded in ADR-0006. | `d054980` |
| R7 | Reuse accepted ignored files and edits hidden by assume-unchanged or skip-worktree. | `TestCheckout_ReuseRefusesHiddenModifications`. | Reuse requires `status --porcelain --ignored --untracked-files=all` to be empty and no lowercase or `S` tag in `ls-files -v`. | `8b2eacb` |
| R8 | A directory created by a failed attempt was never made durable by the successful retry. | `TestApply_RetryAfterFailure_FsyncsParentsOfDirectoriesItDidNotCreate`. Mutation-checked. | The parent of every path component is queued for fsync, whether or not this call created it. | `efc5f43` |
| R9 | A done context or a missing `git` binary was wrapped as the permanent `ErrInvalidRepo`. | `TestCheckout_TransientFailures_NotPermanentSentinels`. | `permanent()` keeps those failures plain and retryable. | `8b2eacb` |
| R10 | A tracked `.f.tollgate.tmp` inside a root was silently deleted when the overlay wrote `f`. | `TestCheckout_TreeWithReservedSuffix_RefusedBeforeCheckout`; `TestActivities_CheckoutWorkspace_ReservedPath_NonRetryable`. | The suffix is reserved workspace-wide (`ports.ReservedPathSuffix`). The checkout refuses such a tree before creating any worktree (`ErrReservedPath`, `ReservedPathInTree`). | `8b2eacb` |
| R11 | A workspace path that is itself a symlink to another job's worktree was reused. | `TestCheckout_WorkspacePathIsASymlink_Refused`. | The workspace path is checked with Lstat, and a symlink is a conflict. | `8b2eacb` |

### Warnings

| Warning | Resolution | Commit |
|---|---|---|
| A budget below the kill margin was reported as unmetered and counted. | New non-retryable `AgentRunBudgetTooShort`, not counted (`TestActivities_RunAgent_BudgetBelowMargin_RefusedNothingSpentNotCounted`). The runner's own "context done before start" path returns a plain not-started error (`TestRunner_Run_ContextDoneBeforeStart_NotStartedNotUnmetered`). The runner's `ctx.Err()` term is pinned by `TestRunner_Run_CancelledRunExitingOnItsOwn_IsUnmetered`, which uses a catchable cancel signal (mutation-checked). | `72fee13`, `d054980` |
| Permanent overlay conflicts came back retryable. | `ErrDestinationConflict`, mapped to `OverlayDestinationConflict`, covering all five cases (`TestApply_DestinationConflicts_RejectedBeforeWriting`, plus an engine attempt-count case). | `efc5f43` |
| `GOOS=windows go build ./...` failed. | Process control and overlay writing sit behind `unix` build tags, with stubs that fail with `errors.ErrUnsupported`, mapped to `UnsupportedPlatform`. | `d054980`, `efc5f43` |
| Uncapped stdout. | 64 MiB cap; overflow is unmetered (`TestRunner_Run_StdoutOverCap_IsUnmetered`). | `d054980` |
| The ledger accepted an empty RunID. | `ErrMissingRunID` (`TestLedger_RecordCosts_EmptyRunID_Rejected`). | `740fb31` |
| Absolute WorkspaceRoot at startup; the `--` in `worktree add` was untested. | `Activities.Validate`, called by the worker (`TOLLGATE_WORKSPACE_ROOT`); the checkout refuses relative paths. `TestCheckout_WorktreeAddTerminatesOptionsBeforeThePath` records the real argv through a git wrapper. | `8b2eacb` |
| The unmetered log fields were mutation-invisible. | `TestActivities_RunAgent_Unmetered_LogCarriesJobAndAttempt` asserts `job_id` and `attempt` through the Temporal test logger. | `72fee13` |
| The retry-cap test compared against the constant itself. | It now asserts the literal 2. | `72fee13` |
| Post-check source swaps, per-file fsync and cancellation between files were mutation-invisible. | `TestApply_SourceSwappedAfterPrecheck_Refused` (symlink, FIFO with a timeout), `TestApply_EveryFileFsyncedBeforeRename`, `TestApply_ContextCancelledBetweenFiles_StopsWriting`. The last one found a real ordering bug: the context is now checked after the per-file heartbeat. | `efc5f43` |
| No GetVersion for the new first activity. | ADR-0006 §10 states why none is needed, since no worker ran the workflow before merge. Replay fixtures (success, billed) were captured after the last workflow change, and a mutation (reordering the first activity) fails the replay. | `7ea599b`, docs |

### Follow-ups (not fixed in this change)

- **Judge calls (`claudecode.CLIJudge`, PR shape only).** Reproduction, as
  described by the reviewer:
  - The judge CLI prints an envelope with `is_error: true`, or a valid
    envelope whose `result` is not verdict JSON, or exits non-zero after
    printing an envelope.
  - `CLIJudge.Judge` returns a plain error. `JudgeOne` passes it to
    Temporal as retryable, the judge is invoked again, and the first call's
    spend is never recorded.
  - The judge also runs with no process group, no `WaitDelay`, no
    `cmd.Dir`, and the CLI's default tools.
  - This predates the artifact-jobs change, and artifact jobs run no
    judges. The judge's paid-run id (R1) is fixed; the rest belongs to a
    judge-hardening change that mirrors the runner's classification.
- **Group-kill PID reuse outside Linux.** On non-Linux Unix systems the
  leader is reaped before its group is killed. Recorded in ADR-0006
  Consequences.
- **Migration 00004 index rebuild.** `CREATE UNIQUE INDEX` takes an
  exclusive lock for the build. That is fine at the current size; a large
  ledger needs a `CONCURRENTLY` migration. Recorded in ADR-0006
  Consequences.
