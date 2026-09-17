# Tasks: Artifact jobs

Delivery: **one PR**, `size:exception` accepted by the user (delivery_strategy=exception-ok).
This document does not propose splitting or cutting scope. Review budget
(800 production lines) is informational only for this run; see the Review
Workload Forecast at the end. Test lines are reported separately and never
count toward it.

Strict TDD is enabled: every task below states its failing test first (name +
exact `go test -run` invocation), then the minimal implementation, then a
verification command. Tasks are grouped into work units = one reviewable
commit each, one purpose, tests shipping with the behavior they verify.

Traceability legend: `[Dn]` = design.md decision Dn. `[Spec: <file>#<Req>]` =
a MUST from one of the four spec files.

---

## Completion status (apply phase)

All 16 tasks complete. Tasks 1-15 each landed as their own commit on branch
`artifact-jobs`; Task 16 is the verification checklist below (no commit).

- [x] Task 1 — capture replay histories — `c458f11`
- [x] Task 2 — ports.AgentConfig / RunError / ModelUnknown — `c3f3635`
- [x] Task 3 — claudecode `buildArgs` — `9b80c54`
- [x] Task 4 — claudecode model resolution + `RunError` — `5863129`
- [x] Task 5 — `runAgentAndRecord` extraction + billed-failure fix — `ac92740`
- [x] Task 6 — `RunAgent` maps `RunError`→`AgentRunBilled`, model on span/ledger — `7c78124`
- [x] Task 7 — `gitcli.Checkout` — `8ee489a`
- [x] Task 8 — `workspace.Apply` / `ValidatePaths` — `eabdf5e`
- [x] Task 9 — `CheckoutWorkspace` / `ApplyOverlay` activities, `withHeartbeat` — `036addc`
- [x] Task 10 — `ArtifactJobWorkflow`, `validate()` — `d3a67ac` (see Deviations note below)
- [x] Task 11 — `piece_id` migration + ledger persistence + per-piece query — `07802b6` (Postgres gap: see note)
- [x] Task 12 — worker wiring (`cmd/worker/main.go`) — `5b08e42`
- [x] Task 13 — replay regression gate against Task 1's fixtures — `2ec7101` (no determinism fix needed)
- [x] Task 14 — E2E test on a real Temporal dev server — `8784fa3`
- [x] Task 15 — docs (`DESIGN.md` §2.1, `docs/artifact-job-cli.md`) — `07b69c2`
- [x] Task 16 — final verification gate — see commands and results below; Postgres gap called out explicitly

**Deviations (documented, not silent)**:
1. Task 1's literal precondition `git rev-parse HEAD == 9a5be05` did not hold (two docs-only SDD commits landed on top); verified instead that `git diff --stat 9a5be05 HEAD` touched zero `.go` files, which is the substantive precondition the ordering constraint protects.
2. Task 10 filled two gaps the original breakdown left open: (a) `RunAgentInput` had no `AgentConfig` field, so nothing threaded `ArtifactJobInput.AgentConfig` into `ports.RunSpec` — added `RunAgentInput.AgentConfig` and wired it in `activities.go`'s `RunAgent`; (b) `CheckoutInput.Path` (Task 9) became `CheckoutInput.JobID`, with `CheckoutWorkspace` computing `<WorkspaceRoot>/tollgate-artifact-<JobID>` itself (mirroring `Prepare`), because D7 ("the git adapter never builds the path") is assigned to Task 10 in the traceability matrix, not Task 9.
3. Task 11: no Postgres instance is provisioned for this repo in the apply environment (a system Postgres is reachable on `localhost:5432` but the `tollgate`/`tollgate` role/db does not exist there and was not created, to avoid touching a shared instance out of scope). All Postgres-backed tests skip cleanly via the existing `TOLLGATE_TEST_DATABASE_URL` / `TOLLGATE_REQUIRE_POSTGRES` rule; the migration was verified with `goose -dir migrations validate` (0 errors) instead of a live apply. This is a environment gap, not a code defect — flagged per Task 16's own instruction rather than silently treated as done.

---

## Ordering constraint (hard, non-negotiable)

**Task 1 must run first, at base commit `9a5be05`, before any production line
of this change exists.** If a single production file is edited before Task 1
lands, the captured histories no longer prove anything about a frozen engine
and the replay task (Task 13) is void — the whole change must restart from
`9a5be05`. Verify `git rev-parse HEAD` equals `9a5be05` and `git status
--porcelain` is empty before starting Task 1.

---

## Work Unit 1 — capture replay histories (test-only, zero production lines)

### Task 1 — Capture `JobWorkflow` ship and reject histories at base commit

**Precondition (verify, do not skip):** `git rev-parse HEAD` == `9a5be05`;
`git status --porcelain` empty; `internal/engine/testdata/` does not exist yet.

**RED / capture step** (this is a data-capture test, not a behavioral RED —
treat "the two files do not exist" as the failing state):
- Create `internal/engine/history_capture_test.go`, gated by
  `TOLLGATE_CAPTURE_HISTORY=1` (skip otherwise, same pattern as
  `TOLLGATE_REQUIRE_TEMPORAL` in `crash_resume_test.go`).
- It dials a real Temporal dev server, runs `JobWorkflow` to completion twice
  on ephemeral task queues with fake activities (same shape as
  `crashActivities` in `crash_resume_test.go`):
  - **ship**: one judge, blocking axis passes → `StatusShipped`.
  - **reject**: one judge, blocking axis fails, `MaxFixAttempts: -1` (no fix
    loop) → `StatusRejected`.
- Fetches each run's full history (`client.GetWorkflowHistory`, all event
  types) and writes it as protojson to:
  - `internal/engine/testdata/jobworkflow_ship.history.json`
  - `internal/engine/testdata/jobworkflow_reject.history.json`

**Run:**
```
TOLLGATE_CAPTURE_HISTORY=1 go test ./internal/engine/... -run TestCaptureJobWorkflowHistories -v
```

**Acceptance criteria (all required):**
1. Both `testdata/*.history.json` files exist and are non-empty valid JSON.
2. `git diff --stat` shows exactly three new files:
   `internal/engine/history_capture_test.go` and the two `testdata/*.json`
   files. Zero lines changed in any existing file.
3. `git diff HEAD -- internal/engine/workflow.go internal/engine/activities.go`
   is empty — no production line exists yet.
4. `go build ./...` and `go test ./... -race` still pass unchanged (the new
   test file is a no-op without the capture env var).

**Verify:** `go test ./... -race && go build ./...`

**Commit:** `test(engine): capture JobWorkflow replay histories at base commit`
(test-only commit, 0 production lines)

Covers: groundwork for `[Spec: artifact-job#Coexistence with the PR-Shaped
JobWorkflow]` scenario "Pre-change history replays cleanly," consumed by
Task 13.

---

## Work Unit 2 — agent port types

### Task 2 — `RunError`, `AgentConfig`, `ModelUnknown`, `ErrInvalidAgentConfig` `[D1, D4, D14]`

**RED:** `internal/ports/agent_test.go`
- `TestRunError_ErrorIncludesUnderlyingMessage`
- `TestRunError_UnwrapsToUnderlyingErr` (via `errors.Is`/`errors.As`)

```
go test ./internal/ports/... -run TestRunError -v
```
(fails to compile: `ports.RunError` does not exist)

**GREEN:** in `internal/ports/agent.go`:
- `RunSpec.AgentConfig json.RawMessage`
- `RunResult.Model string`
- `var ErrInvalidAgentConfig = errors.New(...)`
- `const ModelUnknown = "unknown"`
- `type RunError struct { Result RunResult; Err error }` with `Error() string`
  and `Unwrap() error`

**Verify:** `go test ./internal/ports/... -race && go build ./...`

**Commit:** `feat(ports): add AgentConfig, resolved model, and billed-run error type`

Covers: `[D1]` (sentinel crossing the port boundary), `[D4]` (`Model` field,
`ModelUnknown`), `[D14]` (`RunError` shape).

---

## Work Unit 3 — Claude Code config parsing and argument building

### Task 3 — `claudecode/config.go`: parse `AgentConfig`, build the D2 argument list `[D1, D2, D3]`

**RED:** `internal/adapters/claudecode/config_test.go`, table-driven:
- `TestBuildArgs_EmptyConfig_MatchesPreChangeArgs` — `[Spec: agent-run-config#Empty Config Compatibility]`
- `TestBuildArgs_FullConfig_IncludesModelToolsAllowlist` — `[Spec: agent-run-config#Model and Allowlist Reach the CLI]`
- `TestBuildArgs_AllowedToolsGoesLast`
- `TestBuildArgs_NeverEmitsBypassOrDangerousFlag` — `[Spec: agent-run-config#No Bypass Permission Mode]`
- `TestBuildArgs_UnknownField_RejectedBeforeSpawning`
- `TestBuildArgs_ValueStartingWithDash_Rejected` (`-rule`, `-model` cases —
  threat matrix "Agent argument injection")
- `TestBuildArgs_AlwaysIncludesRestricted`

```
go test ./internal/adapters/claudecode/... -run TestBuildArgs -v
```

**GREEN:** create `config.go`:
- unexported `agentConfig{Model, Tools, AllowedTools}`, parsed with
  `json.NewDecoder(...).DisallowUnknownFields()`
- `buildArgs(prompt string, cfg json.RawMessage) ([]string, error)`:
  empty/`null` → today's args unchanged; non-empty → `-p <prompt>
  --output-format json --model M --restricted --strict-mcp-config --tools
  t1,t2 --permission-mode dontAsk --permission-prompts none --allowedTools r1
  r2 …` (allowedTools last); any value empty or starting with `-` →
  `ports.ErrInvalidAgentConfig`, no process spawned.

**Verify:** `go test ./internal/adapters/claudecode/... -race`

**Commit:** `feat(adapter/claudecode): parse AgentConfig and build restricted CLI arguments`

Covers: `[D1]` (adapter-side parse + reject before spawn), `[D2]` (flag
order, `dontAsk` mode), `[D3]` (`--restricted` always present — permission
*behavior* itself was verified manually against the real CLI per ADR-0006 §5
and is not re-asserted by a unit test), threat matrix row "Agent argument
injection."

---

## Work Unit 4 — Claude Code runner wiring: model resolution, billed failure, stdin

### Task 4 — `claudecode/runner.go`: wire `AgentConfig`, resolve model, return `RunError` on billed failure `[D2, D4, D14]`

**RED:** extend `internal/adapters/claudecode/runner_test.go`:
- `TestRunner_Run_PassesAgentConfigIntoArgs` (fake script dumps `"$@"` to a
  file in the workspace; assert built args match `buildArgs` output)
- `TestRunner_Run_StdinIsDevNull` (fake script: `readlink /proc/self/fd/0`;
  assert the resolved path is `/dev/null`)
- `TestRunner_Run_ModelSelection_HighestCostWins` (two `modelUsage` keys,
  assert the higher-cost id wins)
- `TestRunner_Run_ModelSelection_TiesBreakOnSmallestID`
- `TestRunner_Run_ModelSelection_FallsBackToRequestedAlias` (no `modelUsage`,
  a model was requested)
- `TestRunner_Run_ModelSelection_UnknownWhenNoUsageAndNoAlias` — `[Spec:
  artifact-job / cost-ledger — unknown model label]`
- `TestRunner_Run_PermissionDenialWithExitZero_IsSuccess` (`is_error=false`,
  `permission_denials` populated)
- `TestRunner_Run_IsErrorTrue_ReturnsRunErrorWithPartialResult` — `[Spec:
  cost-ledger#Billed Failure Is Recorded Before the Job Fails]`
- `TestRunner_Run_NonZeroExitWithParseableEnvelope_ReturnsRunError`
- `TestRunner_Run_NonZeroExitWithGarbageOutput_ReturnsPlainError` (stays
  retryable — no `RunError`)

```
go test ./internal/adapters/claudecode/... -run TestRunner_Run -v
```

**GREEN:** modify `runner.go`:
- `cmd.Args` built via `buildArgs(spec.Prompt, spec.AgentConfig)`; on error,
  return `ports.ErrInvalidAgentConfig` before `exec.CommandContext` starts.
- Leave `cmd.Stdin` nil (already true — assert it via the fake script, do not
  add an explicit `/dev/null` open that could shadow the Go default).
- Add `modelUsage` to `resultEnvelope`; implement selection: highest
  `costUSD`, ties broken by smallest id, else the requested alias, else
  `ports.ModelUnknown` with `activity.GetLogger`-style warning left to the
  activity layer (the adapter itself has no activity context — log via a
  plain `log.Printf`-free path: return the value, let `RunAgent` in
  `activities.go` warn — see Task 6).
- On `is_error=true` or non-zero exit with a parseable envelope, return
  `&ports.RunError{Result: ..., Err: fmt.Errorf(...)}`. On non-zero exit with
  unparseable output, return the existing plain error unchanged.

**Verify:** `go test ./internal/adapters/claudecode/... -race`

**Commit:** `feat(adapter/claudecode): resolve run model and surface billed-run errors`

Covers: `[D2]` (wiring), `[D4]` (model resolution), `[D14]` (adapter half of
the billed-failure contract), `[Spec: agent-run-config#Model and Allowlist
Reach the CLI]`.

---

## Work Unit 5 — `JobWorkflow`: extract `runAgentAndRecord`, wire the billed-failure fix

### Task 5 — extract the shared helper; `JobWorkflow`'s error path records a billed row before failing `[D14]`

**RED:** `internal/engine/workflow_test.go` (new cases) + new
`internal/engine/workflow_billed_test.go`:
- `TestRunAgentAndRecord_Success_RecordsRowAndReturnsResult`
- `TestRunAgentAndRecord_AgentRunBilled_RecordsRowThenReturnsError` — mock
  `RunAgent` to return a `temporal.NewNonRetryableApplicationError(msg,
  "AgentRunBilled", agentResultDetails)`; assert `RecordCosts` is called with
  the partial cost/usage/model **before** the function returns the error.
- `TestRunAgentAndRecord_UnparseableFailure_RecordsNothingStaysRetryable` —
  mock `RunAgent` returning a plain (non-`AgentRunBilled`) error; assert
  `RecordCosts` is never called.
- `TestJobWorkflow_BilledFailure_RecordsAgentRowAndNeverCallsJudgeOne` —
  `[Spec: cost-ledger#Billed Failure Is Recorded Before the Job Fails]`
  scenario "PR-shaped JobWorkflow"

```
go test ./internal/engine/... -run 'TestRunAgentAndRecord|TestJobWorkflow_BilledFailure' -v
```

**GREEN:** in `workflow.go`, extract lines 164-177 into:
```go
func runAgentAndRecord(agentCtx, ctx workflow.Context, run RunAgentInput, actor, pieceID string) (AgentResult, error)
```
It executes `RunAgent`, and on an `AgentRunBilled` application error,
extracts the `AgentResult` details, calls `RecordCosts` with that partial
data (model, usage, cost, `pieceID`), and only then returns the error
unchanged (still non-retryable). On success it records and returns normally.
On any other error it returns without recording. Update `JobWorkflow`'s two
call sites (agent run, and — no, `JobWorkflow` has one `RunAgent` call site)
to use the helper with `pieceID: ""`.

**Verify:** `go test ./internal/engine/... -race` — must include unedited
`workflow_test.go` and `crash_resume_test.go` still green (`[Spec:
artifact-job#Coexistence]` scenario "Existing PR-shape tests pass unchanged").

**Commit:** `feat(engine): extract runAgentAndRecord and fix billed-failure accounting on JobWorkflow`

Covers: `[D14]` (workflow-level half), `[Spec: cost-ledger#Billed Failure Is
Recorded Before the Job Fails]` (JobWorkflow scenario), `[Spec: artifact-job
#Coexistence]` (billed-failure fix clause + unchanged-tests scenario).

---

## Work Unit 6 — `activities.go`: `RunAgent` maps `RunError` to `AgentRunBilled`; model on ledger row and span

### Task 6 — activity-level billed-failure mapping, model on `CostEntry` and `gen_ai.response.model` `[D4, D13, D14]`

**RED:** `internal/engine/activities_test.go` (new cases):
- `TestActivities_RunAgent_PortRunError_MapsToNonRetryableAgentRunBilled` —
  fake `ports.AgentRunner` returns `*ports.RunError`; assert the activity
  returns a `temporal.ApplicationError` of type `"AgentRunBilled"`,
  non-retryable, carrying the partial `AgentResult` as details.
- `TestActivities_RunAgent_Success_RecordsModelOnSpan` — assert
  `telemetry.Result.Model` is set and `gen_ai.response.model` is emitted
  (use the in-memory span recorder pattern from `telemetry_test.go`).
- `TestActivities_RunAgent_UnknownModel_LogsWarningNoFailure` — envelope with
  no `modelUsage` and no alias → `AgentResult.Model == ports.ModelUnknown`,
  activity still succeeds.

Also `internal/telemetry/instruments_test.go`:
- `TestRecording_End_SetsGenAIResponseModelWhenPresent`
- `TestRecording_End_OmitsResponseModelWhenEmpty`

```
go test ./internal/engine/... -run TestActivities_RunAgent -v
go test ./internal/telemetry/... -run TestRecording_End -v
```

**GREEN:**
- `internal/telemetry/instruments.go`: add `Result.Model string`; in `End`,
  when `res.Model != ""`, add `semconv.GenAIResponseModel(res.Model)`.
- `internal/engine/activities.go`: `RunAgent` inspects the adapter error; if
  it is `*ports.RunError` (via `errors.As`), builds a non-retryable
  `AgentRunBilled` application error with `AgentResult{CostUSD, Usage, Model}`
  as details, and logs a warning via `activity.GetLogger(ctx)` when
  `Model == ports.ModelUnknown`. On success, populate `AgentResult.Model` and
  pass it to `telemetry.Result.Model`.
- `internal/ports/ledger.go`: add `CostEntry.PieceID string` (needed to
  compile `runAgentAndRecord`'s call to `RecordCosts` with a piece id — this
  is where the field earns its test, at Task 11's postgres layer; here it is
  plumbed through but not yet persisted).

**Verify:** `go test ./internal/engine/... ./internal/telemetry/... -race`

**Commit:** `feat(engine): map billed agent-run errors to AgentRunBilled and record the resolved model`

Covers: `[D4]` (telemetry + ledger model), `[D13]` (`PieceID` field added),
`[D14]` (activity-level mapping), `[Spec: cost-ledger#Model Recorded on
run_agent Rows]`, `[Spec: artifact-job#Coexistence]` clause (b) success-path
data change.

---

## Work Unit 7 — git worktree checkout adapter

### Task 7 — `ports.Checkout` + `gitcli.Checkout`: pinned detached worktree, no hooks `[D5, D6]`

**RED:** `internal/adapters/gitcli/checkout_test.go`, real `git` in
`t.TempDir()` (skip under `-short` if `git` is unavailable — mirror the
existing skip-rule style):
- `TestCheckout_FreshDetachedWorktree` — `[Spec: workspace-preparation#Pinned
  Worktree Checkout]` scenario "Fresh checkout"
- `TestCheckout_RetryReusesMatchingWorktree_NoReClone` — scenario "Retry
  reuses a matching worktree"
- `TestCheckout_ConflictingPath_NonRetryable` (table-driven: different SHA,
  dirty tree, non-git directory) — scenario "Conflicting path fails
  non-retryably"
- `TestCheckout_UnknownSHA_Rejected`
- `TestCheckout_PathNotARepo_Rejected`
- `TestCheckout_RunsNoRepositoryHooks` — install a `post-checkout` hook that
  writes a marker file; assert the marker is absent after checkout —
  `[Spec: workspace-preparation#Pinned Worktree Checkout]` scenario "Checkout
  runs no repository hooks"; threat matrix row "Commit / push state" N/A but
  "Git repository selection" applicable — asserts the hook does not run.

```
go test ./internal/adapters/gitcli/... -run TestCheckout -v
```

**GREEN:**
- `internal/ports/checkout.go`: `type Checkout interface { Checkout(ctx
  context.Context, repo, sha, path string) error }`; sentinels
  `ErrInvalidRepo`, `ErrRefNotFound`, `ErrCheckoutConflict`.
- `internal/adapters/gitcli/checkout.go`: every invocation carries `-c
  core.hooksPath=/dev/null`; probe with `cat-file -e <sha>^{commit}`; fresh
  checkout via `worktree add --detach <ws> <sha>`; reuse only when
  `--git-common-dir` matches, `HEAD` equals the sha, and `status --porcelain`
  is empty; anything else → `ErrCheckoutConflict`.

**Verify:** `go test ./internal/adapters/gitcli/... -race`

**Commit:** `feat(adapter/gitcli): pinned detached-worktree checkout with hooks disabled`

Covers: `[D5, D6]`, `[Spec: workspace-preparation#Pinned Worktree Checkout]`
(all 4 scenarios), threat matrix "Git repository selection."

---

## Work Unit 8 — root-bounded durable overlay

### Task 8 — `workspace.ValidatePaths` and `workspace.Apply` `[D9, D10, D11]`

**RED:** `internal/workspace/overlay_test.go`, real filesystem:
- `TestValidatePaths_TableDriven` — `..`, absolute dest, `.` root, `.git`
  segment, and the `jobsX` vs `jobs` prefix-collision case (destination must
  equal a root or sit below it with a `/` boundary, not merely share a string
  prefix) — `[Spec: artifact-job#Input Validation]` scenarios on roots and
  overlay destinations; `[Spec: workspace-preparation]` implicit precondition.
- `TestApply_OverwriteOfTrackedFile_KeepsMode` — `[Spec:
  workspace-preparation#Durable, Root-Bounded Overlay]` scenario "Overwrite of
  a tracked file inside a root is allowed"
- `TestApply_DestinationSymlinkEscape_NoFileWritten` — scenario "Symlink
  escape rejected"
- `TestApply_SourceSymlinkAnywhereInTree_Rejected_NoFileWritten` — scenario
  "Symlink inside an overlay source rejected"
- `TestApply_RetryAfterPartialFailure_IdenticalTreeNoTempFilesLeft` — inject a
  failure on the second file by making its source unreadable (`chmod 0000`)
  before the first `Apply` call, then restore permissions and retry; assert
  the final tree matches a from-scratch copy byte-for-byte and no
  `.*.tollgate.tmp` files remain — `[Spec: workspace-preparation#Durable,
  Root-Bounded Overlay]` scenario "Retry leaves no partial file"

```
go test ./internal/workspace/... -run 'TestValidatePaths|TestApply' -v
```

**GREEN:** create `internal/workspace/overlay.go`:
- `type Overlay struct { Source, Dest string }`
- `ValidatePaths(roots []string, ovs []Overlay) error` (D9: `filepath.Clean`,
  `filepath.IsLocal`, no `.` root, no `.git` segment, dest equals a root or
  sits below one via `root+"/"` prefix, sources absolute).
- `Apply(ctx, workspace string, roots []string, ovs []Overlay, beat func())
  error`: pre-check pass over every overlay first (D10: `wsRoot.Lstat` each
  existing path component, walk every source tree for symlinks) before any
  write; writes go through `wsRoot.OpenRoot(root)`; each file → `.<base>
  .tollgate.tmp` with `O_TRUNC`, fsync, rename, then fsync the containing
  directory once (D11); preserve mode bits.
- `var ErrOutsideRoots, ErrUnsupportedSource error`

**Verify:** `go test ./internal/workspace/... -race`

**Commit:** `feat(workspace): root-bounded overlay with symlink pre-check and durable writes`

Covers: `[D9, D10, D11]`, `[Spec: workspace-preparation#Durable, Root-Bounded
Overlay]` (all 4 scenarios).

---

## Work Unit 9 — `CheckoutWorkspace` / `ApplyOverlay` activities, `withHeartbeat`

### Task 9 — wire the two new activities with correct timeouts and heartbeats `[D12]`

**RED:** `internal/engine/activities_test.go` (new cases), Temporal test env:
- `TestActivities_CheckoutWorkspace_DelegatesToPort`
- `TestActivities_ApplyOverlay_HeartbeatsDuringLongCopy` — assert `beat` is
  invoked at least once via an injectable heartbeat sink (same seam as
  `Activities.heartbeat`)
- `TestWithHeartbeat_StopsCleanlyOnCompletion` (unit test of the extracted
  helper in isolation)

```
go test ./internal/engine/... -run 'TestActivities_CheckoutWorkspace|TestActivities_ApplyOverlay|TestWithHeartbeat' -v
```

**GREEN:**
- Extract the heartbeat goroutine loop in `activities.go:103-121` into
  `func (a *Activities) withHeartbeat(ctx context.Context, work func() error)
  error`; `RunAgent` is refactored to use it (no behavior change — covered by
  existing `RunAgent` tests staying green).
- `Activities.Checkout ports.Checkout` field.
- `func (a *Activities) CheckoutWorkspace(ctx context.Context, in
  CheckoutInput) (Workspace, error)` — delegates to `a.Checkout.Checkout`.
- `func (a *Activities) ApplyOverlay(ctx context.Context, in OverlayInput)
  error` — calls `workspace.Apply` wrapped in `withHeartbeat`.
- Activity options (wired at the workflow, Task 10): `CheckoutWorkspace` 10
  min / 3 attempts; `ApplyOverlay` 30 min start-to-close / 30 s heartbeat / 3
  attempts.

**Verify:** `go test ./internal/engine/... -race`

**Commit:** `feat(engine): CheckoutWorkspace and ApplyOverlay activities with shared heartbeat helper`

Covers: `[D12]`.

---

## Work Unit 10 — `ArtifactJobWorkflow`

### Task 10 — `engine/artifact.go`: types, `validate()`, orchestration `[D7, D8, D9, D13]`

**RED:** `internal/engine/artifact_test.go`, `testsuite.WorkflowTestSuite`:
- `TestArtifactJobWorkflow_Order_ChecksOutOverlaysRunsAgentRecordsCost` —
  `[Spec: artifact-job#No PR, No Gate]` scenario "Job completes without a PR"
  (assert no `Ship`/`JudgeOne` mock is ever registered as called — the test
  env should fail loudly if either is invoked, since neither is mocked).
- `TestArtifactJobWorkflow_InvalidInput_TableDriven`, one sub-test per case,
  asserting `InvalidInput` and **zero** activities executed:
  branch ref, short SHA, 39-char SHA, 41-char SHA, non-hex char, surrounding
  whitespace, missing root, root `.`, root with `.git` segment, escaping
  destination (`..`), unsafe `job_id`, empty `agent_config`, `null`
  `agent_config` — `[Spec: artifact-job#Input Validation]` (all 11 scenarios)
- `TestArtifactJobWorkflow_UppercaseSHA_ReachesCheckoutLowercased`
- `TestArtifactJobWorkflow_AgentRunBilled_RecordsRowThenFails` — `[Spec:
  cost-ledger#Billed Failure Is Recorded Before the Job Fails]` scenario
  "artifact job"
- `TestArtifactJobWorkflow_OverlayFailure_AgentNeverInvokedNoRunAgentRow` —
  `[Spec: workspace-preparation#Overlay Failure Isolation]`

```
go test ./internal/engine/... -run TestArtifactJobWorkflow -v
```

**GREEN:** create `internal/engine/artifact.go`:
```go
type ArtifactJobInput struct {
  JobID, PieceID, Repo, SourceRef, Prompt string
  AgentConfig json.RawMessage
  DestinationRoots []string
  Overlays []workspace.Overlay
}
type ArtifactJobResult struct { Workspace, Model string; CostUSD float64; Output string }
func validate(in ArtifactJobInput) (ArtifactJobInput, error)
func ArtifactJobWorkflow(ctx workflow.Context, in ArtifactJobInput) (ArtifactJobResult, error)
```
`validate()` is pure: SHA regex + lowercase normalize (reject surrounding
whitespace), `job_id` regex, non-empty `agent_config`, non-empty
`DestinationRoots`, calls `workspace.ValidatePaths`. Returns
`temporal.NewNonRetryableApplicationError(..., "InvalidInput", nil)` on any
violation, before scheduling any activity. The workflow then runs
`CheckoutWorkspace` (10 min/3 attempts) → `ApplyOverlay` (30 min/30 s
heartbeat/3 attempts) → `runAgentAndRecord` (from Task 5, with `pieceID:
in.PieceID`) → assembles `ArtifactJobResult`.

**Verify:** `go test ./internal/engine/... -race`

**Commit:** `feat(engine): add ArtifactJobWorkflow (checkout, overlay, run agent, record cost — no PR)`

Covers: `[D7, D8, D9, D13]`, `[Spec: artifact-job#Input Validation]` (all 11
scenarios), `[Spec: artifact-job#No PR, No Gate]` (workflow-level scenario),
`[Spec: workspace-preparation#Overlay Failure Isolation]`, `[Spec:
cost-ledger#Billed Failure Is Recorded Before the Job Fails]` (artifact-job
scenario).

---

## Work Unit 11 — ledger `piece_id` column

### Task 11 — migration `00004` + `postgres.Ledger` writes `piece_id` `[D13]`

**RED:** extend `internal/adapters/postgres/ledger_test.go` (uses the
existing `testPool` skip rule: `TOLLGATE_TEST_DATABASE_URL`,
`TOLLGATE_REQUIRE_POSTGRES` fails loudly, otherwise `t.Skip`):
- `TestLedger_RecordCosts_PieceIDGiven_RecordedOnEveryRow` — `[Spec:
  cost-ledger#Nullable piece_id Outside the Natural Key]` scenario "piece_id
  recorded when given"
- `TestLedger_RecordCosts_PieceIDOmitted_NullOnEveryRow` — scenario "piece_id
  NULL when omitted"
- `TestLedger_PerPieceSpend_SumsAcrossJobsIncludingFailed` — a documented
  `GROUP BY piece_id, model` query, exercised against 3 jobs sharing one
  `piece_id` where one job only has a `run_agent` row (simulating a job that
  failed after the agent ran) — `[Spec: cost-ledger#Per-Piece Spend
  Aggregation]`

```
go test ./internal/adapters/postgres/... -run 'TestLedger_RecordCosts_PieceID|TestLedger_PerPieceSpend' -v
```

**GREEN:**
- `migrations/00004_cost_entries_piece_id.sql`:
  ```sql
  -- +goose Up
  ALTER TABLE cost_entries ADD COLUMN piece_id TEXT;
  CREATE INDEX cost_entries_piece_id_idx ON cost_entries (piece_id) WHERE piece_id IS NOT NULL;
  -- +goose Down
  DROP INDEX cost_entries_piece_id_idx;
  ALTER TABLE cost_entries DROP COLUMN piece_id;
  ```
- `internal/adapters/postgres/ledger.go`: add `piece_id` to the `INSERT`,
  using `NULLIF($11,'')` so an empty string (not just Go's zero value) also
  normalizes to `NULL`.
- Document the per-piece query (as a comment or a small helper) —
  `SELECT piece_id, model, SUM(usd) FROM cost_entries WHERE piece_id IS NOT
  NULL GROUP BY piece_id, model`.

**Verify:** `go test ./internal/adapters/postgres/... -race` (skips cleanly
without a reachable Postgres, per the existing rule; run for real with
`TOLLGATE_TEST_DATABASE_URL` set, or `TOLLGATE_REQUIRE_POSTGRES=1` in CI to
fail loudly instead of silently skipping).

**Commit:** `feat(ledger): add nullable piece_id column and per-piece spend query`

Covers: `[D13]`, `[Spec: cost-ledger#Nullable piece_id Outside the Natural
Key]`, `[Spec: cost-ledger#Per-Piece Spend Aggregation]`.

---

## Work Unit 12 — worker wiring

### Task 12 — register `ArtifactJobWorkflow` and its activities in `cmd/worker/main.go`

**RED:** no new behavioral test (pure wiring); the acceptance gate is
`go build ./...` and a smoke boot. Add
`internal/engine/artifact_wiring_test.go`:
- `TestArtifactJobWorkflow_RegistersAgainstRealActivityStruct` — a
  `worker.New(...).RegisterWorkflow(ArtifactJobWorkflow)` +
  `RegisterActivity(&Activities{...})` call that would panic on a name
  mismatch between the workflow's `workflow.ExecuteActivity(ctx,
  acts.CheckoutWorkspace, ...)` references and the registered struct.

```
go test ./internal/engine/... -run TestArtifactJobWorkflow_RegistersAgainstRealActivityStruct -v
```

**GREEN:** `cmd/worker/main.go`: add `Checkout: &gitcli.Checkout{}` to the
wired `Activities`, `w.RegisterWorkflow(engine.ArtifactJobWorkflow)`.

**Verify:** `go build ./... && go test ./... -race`

**Commit:** `feat(worker): register ArtifactJobWorkflow and the checkout adapter`

Covers: file-changes row `cmd/worker/main.go`; enables `[Spec: artifact-job
#CLI Submission]`.

---

## Work Unit 13 — replay regression gate

### Task 13 — replay the captured histories against the final `JobWorkflow` code `[Spec: artifact-job#Coexistence]`

**RED:** `internal/engine/workflow_replay_test.go`:
- `TestJobWorkflow_ReplaysShipHistory`
- `TestJobWorkflow_ReplaysRejectHistory`

Both use `worker.NewWorkflowReplayer()`, `RegisterWorkflow(JobWorkflow)`, and
`ReplayWorkflowHistoryFromJSONFile` against
`testdata/jobworkflow_{ship,reject}.history.json` from Task 1.

```
go test ./internal/engine/... -run TestJobWorkflow_Replays -v
```

**GREEN:** none expected — if this fails, the extraction in Task 5
introduced non-determinism (wrong signal ordering, a changed activity name,
or an extra/removed step) and must be fixed in `workflow.go`, not worked
around here. This task's only "production" edit, if any, is a determinism
bugfix, not a feature.

**Verify:** `go test ./internal/engine/... -race`

**Commit:** `test(engine): replay ship and reject histories against the final JobWorkflow`
(test-only commit; if a determinism fix was needed, split it into its own
preceding `fix(engine): ...` commit)

Covers: `[Spec: artifact-job#Coexistence with the PR-Shaped JobWorkflow]`
scenario "Pre-change history replays cleanly."

---

## Work Unit 14 — end-to-end test

### Task 14 — real Temporal dev server E2E for `ArtifactJobWorkflow` `[Spec: artifact-job#No PR, No Gate — E2E scenario]`

**RED:** `internal/engine/artifact_e2e_test.go`, modeled on
`crash_resume_test.go:75-133` (same `client.Dial` / skip-unless-
`TOLLGATE_REQUIRE_TEMPORAL` pattern):
- `TestArtifactJobWorkflow_E2E_RealServer_RecordsCostNoPR` — real dev server,
  a real git temp repo (fixture commit), a real overlay onto a scratch root,
  `claudecode.Runner` pointed at a fake `claude` binary (from the Task 4
  fixture pattern) reporting a normal success envelope. Asserts: workflow
  completes, `run_agent` cost rows exist with the model and `piece_id` (via a
  real `postgres.Ledger` when reachable, else an in-memory
  `ports.LedgerStore` fake recording the same fields), and neither `Ship` nor
  `JudgeOne` is registered on the worker at all (so any accidental call would
  fail the workflow, not silently succeed).
- `TestArtifactJobWorkflow_E2E_BilledFailure_RowStillLands` — the fake binary
  returns `is_error=true` with a valid envelope; assert the `run_agent` row
  still lands before the workflow fails.

```
TOLLGATE_REQUIRE_TEMPORAL=1 go test ./internal/engine/... -run TestArtifactJobWorkflow_E2E -v
```

**GREEN:** none expected — this test exercises Work Units 5-12 as already
implemented; a failure here means a wiring gap, fixed in the relevant earlier
unit, not a new feature added at this stage.

**Verify:** `go test ./internal/engine/... -race` (skips cleanly without a
reachable dev server, exactly like `crash_resume_test.go`)

**Commit:** `test(engine): end-to-end ArtifactJobWorkflow run on a real Temporal dev server`

Covers: `[Spec: artifact-job#No PR, No Gate]` scenario "End-to-end run on a
live Temporal server."

---

## Work Unit 15 — docs

### Task 15 — `docs/DESIGN.md` update and the `temporal workflow start` payload

**No test** (documentation). Acceptance criteria instead of a `go test`
invocation:
1. `docs/DESIGN.md` gains a short section describing the second job shape
   (`ArtifactJobWorkflow`: checkout → overlay → run agent → record cost, no
   judges/gate/Ship) and the frozen-engine boundary via destination roots,
   cross-linking ADR-0006.
2. A new `docs/artifact-job-cli.md` (or a subsection of `docs/DESIGN.md`, an
   authoring choice at implementation time) contains the exact command:
   ```
   temporal workflow start \
     --task-queue tollgate-jobs \
     --type ArtifactJobWorkflow \
     --input '{
       "JobID": "piece-42-plan",
       "PieceID": "piece-42",
       "Repo": "/abs/path/to/local/clone",
       "SourceRef": "3f2b1c9d4e5a6b7c8d9e0f1a2b3c4d5e6f708192",
       "Prompt": "Draft the plan for piece 42.",
       "AgentConfig": {"model": "sonnet", "tools": ["Read", "Write"], "allowed_tools": ["Read", "Write"]},
       "DestinationRoots": ["output/piece-42"],
       "Overlays": [{"Source": "/abs/path/prepared/plan.md", "Dest": "output/piece-42/plan.md"}]
     }'
   ```
   This exact JSON (with the schema's real field names as implemented in
   Task 10) must be verified to be accepted by a live dev server as part of
   Task 14 or a manual check before this task is marked done — do not
   document an untested payload shape.

**Verify:** `golangci-lint run` (docs changes must not break markdown-adjacent
lint if any is configured) and a manual read-through against the final
`ArtifactJobInput` struct field names/JSON tags from Task 10.

**Commit:** `docs: document ArtifactJobWorkflow shape and CLI submission payload`

Covers: `[Spec: artifact-job#CLI Submission]`.

---

## Work Unit 16 — final verification (no new production or test code expected)

### Task 16 — full-suite gate

**Commands, all must pass at the final commit:**
```
go build ./...
go test ./... -race
golangci-lint run
TOLLGATE_REQUIRE_TEMPORAL=1 go test ./internal/engine/... -race   # if a dev server is available in this environment
TOLLGATE_REQUIRE_POSTGRES=1 go test ./internal/adapters/postgres/... -race  # if Postgres is available
```
If a dev server or Postgres is not available in the execution environment,
the plain skip-based runs (no `REQUIRE_*` env vars) must still pass, and the
gap must be called out explicitly rather than silently treated as "done."

No commit — this is a checklist, not a change.

---

## Parallelizable vs sequential

Sequential (each depends on the previous production code existing):
Task 1 → Task 2 → Task 3 → Task 4 → Task 5 → Task 6 → Task 9 → Task 10 →
Task 12 → Task 13 → Task 14 → Task 15 → Task 16.

Task 7 (gitcli) and Task 8 (workspace overlay) depend only on Task 2's port
types being available for compilation ordering convenience, not on each
other or on Tasks 3-6; **they may run in parallel with each other and with
Work Units 3-6**, but both must land before Task 9 (which wires them into
activities) and Task 10 (which wires them into the workflow).
Task 11 (postgres/migration) depends only on Task 6 (which adds
`CostEntry.PieceID`) and can run in parallel with Tasks 7-10.

Everything from Task 9 onward is sequential: each later task's RED test
assumes the previous task's GREEN implementation compiles.

Given `size:exception` and one-PR delivery, "parallel" here means parallel
authorship of independent commits before final ordering/rebase into one PR
branch, not parallel review or parallel merge.

---

## Traceability matrix (every MUST and every D-decision, orphan-checked)

| Spec requirement (MUST) | Task(s) |
|---|---|
| artifact-job: Input Validation (11 scenarios) | 10 |
| artifact-job: No PR, No Gate (incl. E2E scenario) | 10, 14 |
| artifact-job: Coexistence with PR-Shaped JobWorkflow | 1, 5, 6, 13 |
| artifact-job: CLI Submission | 12, 15 |
| workspace-preparation: Pinned Worktree Checkout | 7 |
| workspace-preparation: Durable, Root-Bounded Overlay | 8 |
| workspace-preparation: Overlay Failure Isolation | 10 |
| agent-run-config: Model and Allowlist Reach the CLI | 3, 4 |
| agent-run-config: No Bypass Permission Mode | 3 |
| agent-run-config: Empty Config Compatibility | 3 |
| cost-ledger: Model Recorded on run_agent Rows | 6 |
| cost-ledger: Nullable piece_id Outside the Natural Key | 11 |
| cost-ledger: Billed Failure Is Recorded Before the Job Fails | 4, 5, 6, 10 |
| cost-ledger: Per-Piece Spend Aggregation | 11 |

| Design decision | Task(s) |
|---|---|
| D1 (AgentConfig sentinel, no first-class fields) | 2, 3 |
| D2 (Claude Code flag mapping) | 3, 4 |
| D3 (permission-behavior verification, `--restricted` presence) | 3 |
| D4 (resolved model, ModelUnknown) | 2, 4, 6 |
| D5 (absolute repo, hooks disabled on every invocation) | 7 |
| D6 (reuse rules, ErrCheckoutConflict) | 7 |
| D7 (workspace path / job_id regex in validate()) | 10 |
| D8 (SHA exact-length, no trim, lowercase) | 10 |
| D9 (path validation rules, ValidatePaths) | 8, 10 |
| D10 (pre-check before any write) | 8 |
| D11 (tmp + fsync + rename durability) | 8 |
| D12 (activity boundaries, timeouts, withHeartbeat) | 9 |
| D13 (piece_id nullable column, GROUP BY query) | 6, 11 |
| D14 (billed-failure non-retryable accounting) | 2, 4, 5, 6 |

No spec MUST and no design decision is left uncovered above.

---

## Review Workload Forecast

**Delivery strategy:** `exception-ok` — the user explicitly accepted a
`size:exception` for this change. This run proceeds as **one PR** without
proposing a split or cutting scope, per the session preflight. The 800-line
production budget below is informational only.

**Production lines (tests excluded), my own count by work unit:**

| Work unit | File(s) | ~Lines |
|---|---|---|
| WU2 | `internal/ports/agent.go` | 30 |
| WU3 | `internal/adapters/claudecode/config.go` | 90 |
| WU4 | `internal/adapters/claudecode/runner.go` | 85 |
| WU5 | `internal/engine/workflow.go` | 55 |
| WU6 | `internal/engine/activities.go`, `internal/telemetry/instruments.go`, `internal/ports/ledger.go` | 85 + 8 + 4 |
| WU7 | `internal/ports/checkout.go`, `internal/adapters/gitcli/checkout.go` | 25 + 130 |
| WU8 | `internal/workspace/overlay.go` | 180 |
| WU9 | `internal/engine/activities.go` (CheckoutWorkspace/ApplyOverlay/withHeartbeat, additive to WU6's edit) | included above in the 85 |
| WU10 | `internal/engine/artifact.go` | 120 |
| WU11 | `internal/adapters/postgres/ledger.go`, `migrations/00004_cost_entries_piece_id.sql` | 4 + 12 |
| WU12 | `cmd/worker/main.go` | 6 |
| **Total production** | | **≈834** |

This matches the design's own independent estimate (~834), which is itself
above the orchestrator's ~740 and the original proposal's ~550 — the
proposal never counted the D14 billed-failure path, `claudecode/config.go`,
`internal/adapters/gitcli`, the `withHeartbeat` extraction, the telemetry
model field, or the D10 pre-check walk. I did not find grounds to estimate
lower; if anything the table-driven RED tests in Tasks 3, 8, and 10 push
toward the upper end of each file's range once written against real
signatures.

**Test lines (reported separately, never counted toward the 800 budget):**

| Work unit | Test file(s) | ~Lines |
|---|---|---|
| WU1 | `internal/engine/history_capture_test.go` + 2 JSON fixtures (fixtures excluded from "lines of test code") | 150 |
| WU2 | `internal/ports/agent_test.go` | 40 |
| WU3 | `internal/adapters/claudecode/config_test.go` | 160 |
| WU4 | `internal/adapters/claudecode/runner_test.go` (additions) | 190 |
| WU5 | `internal/engine/workflow_billed_test.go` + `workflow_test.go` additions | 130 |
| WU6 | `internal/engine/activities_test.go` + `internal/telemetry/instruments_test.go` (additions) | 120 |
| WU7 | `internal/adapters/gitcli/checkout_test.go` | 220 |
| WU8 | `internal/workspace/overlay_test.go` | 260 |
| WU9 | `internal/engine/activities_test.go` (additions) | 70 |
| WU10 | `internal/engine/artifact_test.go` | 300 |
| WU11 | `internal/adapters/postgres/ledger_test.go` (additions) | 70 |
| WU12 | `internal/engine/artifact_wiring_test.go` | 20 |
| WU13 | `internal/engine/workflow_replay_test.go` | 90 |
| WU14 | `internal/engine/artifact_e2e_test.go` | 190 |
| **Total test** | | **≈2010** |

Test code is roughly 2.4x the production diff — expected for a change this
security-sensitive (path traversal, symlink escape, argument injection, git
hook suppression, replay determinism) and consistent with the design's own
note that tests "are expected to be larger than the production diff."

**Decision:** proceed under the accepted `size:exception`; no stop, no split
proposal, per `delivery_strategy=exception-ok`.
