# Verify Report — artifact-jobs

Repo `tollgate`, branch `artifact-jobs`, HEAD `d55a2e6`, base `9a5be05`. Strict TDD mode. Verdict: **PASS WITH WARNINGS**.

## Command evidence (all re-run, not trusted from apply)

| Command | Result |
|---|---|
| `go build ./...` | exit 0 |
| `go test ./... -race -count=1` | all 8 packages `ok` (0 failures) |
| `golangci-lint run` | `0 issues.` |
| `TOLLGATE_REQUIRE_POSTGRES=1 go test ./internal/adapters/postgres/... -race -count=1` (real Postgres, port 55432) | 5/5 ledger tests PASS |
| `TOLLGATE_REQUIRE_TEMPORAL=1 go test ./internal/engine/... -race -count=1` (real dev server, port 7233) | all engine tests PASS, incl. both new E2E tests and the two replay tests |

## Spec coverage (every MUST mapped to a passing test)

**artifact-job**: Input Validation (11 scenarios) → `TestArtifactJobWorkflow_InvalidInput_TableDriven` (10 subcases) + `TestArtifactJobWorkflow_UppercaseSHA_ReachesCheckoutLowercased` (valid-passes case) — `internal/engine/artifact_test.go:96,142`. No PR/No Gate → `TestArtifactJobWorkflow_Order_ChecksOutOverlaysRunsAgentRecordsCost` (`AssertNotCalled Ship/JudgeOne`) + `TestArtifactJobWorkflow_E2E_RealServer_RecordsCostNoPR` (live server). Coexistence → `workflow_test.go`/`crash_resume_test.go` pass unedited; `TestJobWorkflow_ReplaysShipHistory`/`ReplaysRejectHistory` (`workflow_replay_test.go:33`) replay Task-1's base-commit histories via `WorkflowReplayer`. CLI Submission → `docs/artifact-job-cli.md` documents a manual live-server run; **no automated test drives the literal `temporal workflow start` CLI** — see MISSING TESTS.

**workspace-preparation**: Pinned Worktree Checkout (4 scenarios) → `TestCheckout_FreshDetachedWorktree`, `TestCheckout_RetryReusesMatchingWorktree_NoReClone`, `TestCheckout_ConflictingPath_NonRetryable` (3-case table), `TestCheckout_RunsNoRepositoryHooks` — `internal/adapters/gitcli/checkout_test.go`, real git in `t.TempDir()`. Durable Root-Bounded Overlay (4 scenarios) → `TestApply_RetryAfterPartialFailure_IdenticalTreeNoTempFilesLeft`, `TestApply_OverwriteOfTrackedFile_KeepsMode`, `TestApply_DestinationSymlinkEscape_NoFileWritten`, `TestApply_SourceSymlinkAnywhereInTree_Rejected_NoFileWritten` — `internal/workspace/overlay_test.go`. Overlay Failure Isolation → `TestArtifactJobWorkflow_OverlayFailure_AgentNeverInvokedNoRunAgentRow`.

**agent-run-config**: Model/Allowlist Reach CLI → `TestBuildArgs_FullConfig_IncludesModelToolsAllowlist`, `TestBuildArgs_AllowedToolsGoesLast`. No Bypass → `TestBuildArgs_NeverEmitsBypassOrDangerousFlag`, `TestBuildArgs_AlwaysIncludesRestricted`; confirmed independently by `grep -rn bypass internal/ cmd/` returning only the test's `NotContains` assertion — zero occurrences in production code. Empty Config Compatibility → `TestBuildArgs_EmptyConfig_MatchesPreChangeArgs` (table: nil/empty-RawMessage/null, all three), independently diffed against `git show 9a5be05:internal/adapters/claudecode/runner.go` — pre-change command was exactly `["-p", prompt, "--output-format", "json"]`, matches.

**cost-ledger**: Model Recorded → `TestLedger_RecordCosts_PersistsAndAggregates` (real Postgres) + `TestActivities_RunAgent_Success_RecordsModelOnSpan`. Nullable piece_id → `TestLedger_RecordCosts_PieceIDGiven_RecordedOnEveryRow` / `PieceIDOmitted_NullOnEveryRow` (real Postgres). Billed Failure (3 scenarios, both job shapes + unparseable) → `TestRunAgentAndRecord_AgentRunBilled_RecordsRowThenReturnsError`, `TestRunAgentAndRecord_UnparseableFailure_RecordsNothingStaysRetryable` (`env.AssertNotCalled(t,"RecordCosts",...)`), `TestArtifactJobWorkflow_AgentRunBilled_RecordsRowThenFails`, `TestJobWorkflow_BilledFailure_RecordsAgentRowAndNeverCallsJudgeOne` — `internal/engine/workflow_billed_test.go:96` is the direct, non-vacuous "unparseable" test. Per-Piece Spend → `TestLedger_PerPieceSpend_SumsAcrossJobsIncludingFailed` (real Postgres).

All cited tests were re-executed in this session, not taken on the apply report's word.

## Deviations checked against the four claims

(a) Task 1 precondition — VERIFIED SOUND. `git rev-parse HEAD` at Task-1 time was not literally `9a5be05` (2 docs-only SDD commits sat on top); `git diff --stat 9a5be05..d55a2e6` confirms the substituted check — the 0.go-files-touched invariant — genuinely held, since the two extra commits (`0376a65`, `0af0e93`) are docs/openspec-only.

(b) D7 "git adapter never builds the path" — VERIFIED SOUND. `internal/adapters/gitcli/checkout.go`: `Checkout.Checkout(ctx, repo, sha, path string)` takes `path` as a parameter, builds nothing. `internal/engine/activities.go:197`: `CheckoutWorkspace` computes `path := filepath.Join(a.workspaceRoot(), "tollgate-artifact-"+in.JobID)` before calling the port. Matches design.md D7 exactly.

(c) gitcli retry test uses `git worktree list --porcelain` comparison instead of a marker file — CONFIRMED, this is a real difference from a stronger alternative (an untracked marker would prove the reuse path never touched the directory; the porcelain compare only proves git's own worktree registry didn't change). Not a weakening: the code path is unambiguous (an `os.Stat` branch decides fresh-vs-reuse before any `worktree add` call), and the assertion is not vacuous — it fails if a duplicate worktree entry were added. Correctly classified as a **WARNING**, not a blocker; this deviation is not called out in apply-progress's "Deviations" list and should have been.

(d) empty-agent-config test uses Go `nil` rather than `json.RawMessage("")` — **PARTIALLY INCORRECT AS STATED**. At the adapter layer (`internal/adapters/claudecode/config_test.go:13-25`), `TestBuildArgs_EmptyConfig_MatchesPreChangeArgs` is table-driven over all three of `nil`, `json.RawMessage("")`, and `json.RawMessage("null")` — the literal empty-string case IS covered here. Only the workflow-level `validate()` table (`artifact_test.go:96`) uses `nil`/`null` and skips the empty-string case, with an inline comment explaining why (`json.RawMessage("")` cannot survive a real client's JSON round-trip, so it is not a reachable input at that layer). This reasoning is sound and documented; not a weakening.

## WARNINGS

1. **Undocumented deviation**: (c) above is real but missing from apply-progress's "Deviations" section, which claims only 3 deviations. Cosmetic — no behavior gap.
2. **CLI-submission requirement not automation-tested**: `artifact-job#CLI Submission` scenario "Start via CLI" says an operator runs literal `temporal workflow start`. The only proof is a manual, ephemeral, deleted test run recorded in prose (`docs/artifact-job-cli.md`, apply-progress). No regression test exercises the `temporal` binary or the exact JSON shape going forward. See MISSING TESTS.

## MISSING TESTS (untested-but-plausibly-correct, not blockers)

1. `workspace-preparation#Durable, Root-Bounded Overlay` scenario "Retry leaves no partial file": `TestApply_RetryAfterPartialFailure_IdenticalTreeNoTempFilesLeft` asserts the *final* state after a successful retry matches source with no leftover temp files, but never inspects the *intermediate* state right after the first (failed) attempt to directly confirm no half-written file existed at that moment. The per-file temp-then-rename design makes this very likely true; it is simply not directly asserted.
2. No automated test drives the actual `temporal workflow start --input '...'` CLI end-to-end (see WARNING 2). The Go-client E2E tests (`artifact_e2e_test.go`) prove the workflow itself, not the CLI submission path literally.

## NOT VALIDATED

- The "byte-identical" live CLI payload claim in `docs/artifact-job-cli.md` (run on 2026-09-16 with a since-deleted test file) — cannot be independently re-run in this session; taken as documented, unverifiable prose.

## Scope

`git diff --stat 9a5be05..HEAD`: 43 files, all within the planned domains (ports, adapters/claudecode, adapters/gitcli, adapters/postgres, engine, workspace, telemetry, migrations, cmd/worker, docs, openspec). No stray/unrelated file changes found.

## Task/spec completion

All 16 tasks in `tasks.md` are checked complete with commit SHAs; task completion matches the commit log on `artifact-jobs` (`c458f11`..`d55a2e6`, 16 commits). No unchecked tasks.
