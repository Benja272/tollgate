# Design: Artifact jobs

## Technical Approach

This change adds `ArtifactJobWorkflow`, a second thin workflow. Its lasting decisions are recorded in `docs/adr/0006-artifact-jobs.md`.

The workflow runs these steps in order:

1. **Validate the input** in pure code.
2. **`CheckoutWorkspace`** runs a git adapter behind a new port.
3. **`ApplyOverlay`** runs `internal/workspace` through `os.Root`.
4. **`runAgentAndRecord`** runs the agent and records its cost. It is a helper shared with `JobWorkflow`, extracted from `workflow.go:164-177`. It records the `run_agent` row for successful runs and for billed failed runs.

Other points:

- **Agent config.** It is an opaque block that only the Claude Code adapter parses (ADR-0002, `0002:42-43`).
- **Claude Code flags.** Every flag the adapter uses was verified against Claude Code 2.1.273 on 2026-09-16, with `--help` and three real headless runs whose effects were checked on disk.
- **Success path of the PR shape.** Its command sequence does not change, and a replay test proves it. The only change is to data: its `run_agent` rows now carry the model, and its spans gain `gen_ai.response.model`.
- **Error path of the PR shape.** It gains the billed-failure accounting fix (D14).
- **Size.** The user accepted a `size:exception`, so this ships as one PR over the 800-line production budget. Scope is not cut to fit.

## Architecture Decisions

| # | Decision | Rejected | Why |
|---|---|---|---|
| D1 | `RunSpec.AgentConfig json.RawMessage`. The adapter parses it with `DisallowUnknownFields`. If parsing fails, it returns `ports.ErrInvalidAgentConfig` before spawning anything, and `RunAgent` turns that into a non-retryable `AgentConfig` error. | First-class fields on `RunSpec` | They would contradict ADR-0002. The adapter must not import Temporal, so a port sentinel crosses the boundary instead. |
| D2 | Claude Code config: `{model, tools, allowed_tools}`. A non-empty config maps to `-p <prompt> --output-format json [--model M] --restricted --strict-mcp-config [--tools t1,t2] --permission-mode dontAsk --permission-prompts none [--allowedTools r1 r2 …]`. `--allowedTools` goes last because it takes several values. An empty or `null` config produces exactly today's arguments (`runner.go:53`). `cmd.Stdin` stays nil, which Go connects to `/dev/null`; without it the CLI waits 3 s for stdin. | A `default` permission mode; skipping `--restricted` | `default` no longer exists (the valid modes are acceptEdits, auto, bypassPermissions, manual, dontAsk, plan). Without `--restricted`, the pinned repo's settings files could widen the allowlist. |
| D3 | Permission behaviour, verified: under `dontAsk` plus `--permission-prompts none`, a tool that is not allowed is denied. The run does not hang, exits 0 with `is_error=false`, and lists the tool in `permission_denials`. `Bash(<cmd> *)` rules work. Commands the CLI classifies as read-only (for example `echo`) run even without a Bash rule. Under `--restricted`, Bash exists only if `tools` names it. | — | The allowlist bounds mutating commands; the `tools` list bounds whether code can run at all. |
| D4 | `RunResult.Model` is the **resolved** id: the `modelUsage` key with the highest `costUSD`, with the smallest id breaking ties. The requested alias is used only when `modelUsage` is absent. (Verified: the keys are ids such as `claude-haiku-4-5-20251001`.) With no `modelUsage` **and** no alias (the empty-config PR shape), the model becomes the constant `ports.ModelUnknown = "unknown"`. A billed run never fails over a missing label, and `RunAgent` logs a warning through `activity.GetLogger`. | Recording the alias; failing the run; leaving the field empty | Aliases move over time. An empty field would break the spec's "every row records the model". The adapter stays free of logging, which stays on the engine side. |
| D5 | `repo` is an absolute path to a local clone. **Every** git invocation carries `-c core.hooksPath=/dev/null`, because `worktree add` fires `post-checkout`, not only the `cat-file` probe. The adapter runs `git -c core.hooksPath=/dev/null -C <repo> cat-file -e <sha>^{commit}`, then `git -c core.hooksPath=/dev/null -C <repo> worktree add --detach <ws> <sha>`, and the same for the reuse probes. | A URL plus a clone cache | No credentials or fetch policy are needed, and worktrees share the object store. |
| D6 | An existing path is reused only if its `--git-common-dir` equals the repo's, its `HEAD` equals the SHA, and `status --porcelain` is empty. Anything else returns `ErrCheckoutConflict`. | `reset --hard` | A reset would destroy artifacts that were handed off. |
| D7 | Workspace path: `<WorkspaceRoot>/tollgate-artifact-<job_id>`. `job_id` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, checked in `validate()` inside the workflow, before any activity runs. The git adapter never builds the path. | Using the raw id; checking inside the activity | A raw id could traverse paths. Checking in `validate()` keeps every input rule in one pure function, so a bad id costs nothing. |
| D8 | `source_ref` must match `^[0-9a-fA-F]{40}$` exactly: no trimming, so surrounding whitespace fails. `validate()` returns it lowercased, and every later step uses that value. | Rejecting uppercase | Aligns with the spec. |
| D9 | Paths, checked after `filepath.Clean`. At least one root is required. Each root must pass `filepath.IsLocal`, must not be `.`, and must have no `.git` component. Each destination follows the same rules and must equal a root or sit below one (`r+"/"` prefix). Sources must be absolute. `workspace.ValidatePaths` is pure and shared by the workflow and `Apply`. | Allowing `.` | `.` exposes the engine. |
| D10 | Before **any** write, a pre-check runs over every overlay. It calls `wsRoot.Lstat` on each existing component of `root/dest`, and walks every source tree. Any symlink in either place returns `ErrOutsideRoots`, which is non-retryable, so no file has been written. Writes then go through `wsRoot.OpenRoot(root)` as a second line of defence. | Checking each file as it is written | The spec requires "no file is written" when a destination is rejected. |
| D11 | Durability: each file is written to `.<base>.tollgate.tmp` with `O_TRUNC`, fsynced, and renamed into place. Each touched directory is fsynced once, and permission bits are preserved. A retry rewrites everything. | A temp name per attempt | It leaves orphan files. |
| D12 | `ApplyOverlay` is its own activity: 30 min start-to-close, 30 s heartbeat, 3 attempts. The heartbeat loop in `activities.go:103-121` is extracted into `withHeartbeat`. `CheckoutWorkspace` gets 10 min and 3 attempts. | Doing the overlay inside `RunAgent` | ADR-0001, and `activities.go:192-194`. |
| D13 | `CostEntry.PieceID` maps to `piece_id TEXT NULL`, written as `NULLIF($11,'')`, with a partial index. It stays outside the natural key (`00002:5-6`). Per-piece totals use a documented `GROUP BY piece_id, model` query. New types use snake_case JSON tags; new fields on existing types are `omitempty`. | A Go aggregation port; tagging `JobInput` | Nothing needs the port. Tagging `JobInput` would change the PR-shape payloads. |
| D14 | **Billed failure.** When stdout holds a parseable envelope and either `is_error=true` or the exit code is non-zero, the runner returns `*ports.RunError{Result, Err}` carrying the partial cost, usage and model. `RunAgent` maps it to a **non-retryable** `ApplicationError` of type `AgentRunBilled`, with the partial `AgentResult` as details. `runAgentAndRecord` extracts the details, records the `run_agent` row, and then returns the error. With no parseable envelope (the process crashed or was killed), there is nothing to record, and the error stays retryable as it is today. Both job shapes get this. | Returning success with a failure flag; keeping billed errors retryable | Keeping billed errors retryable means a retried activity surfaces only its last attempt's error, so the earlier attempt's spend is lost. It also bills again by accident (`workflow.go:22-26`). |

## Data Flow

    temporal workflow start --type ArtifactJobWorkflow --input '{…}'
      │  validate() — pure; on failure: InvalidInput (non-retryable), no activity runs
      ├─→ CheckoutWorkspace ─→ ports.Checkout (adapters/gitcli)
      ├─→ ApplyOverlay ─→ workspace.Apply (pre-check all, then tmp + fsync + rename)
      ├─→ runAgentAndRecord: RunAgent ─→ claudecode (--restricted, dontAsk, prompts none)
      │     ok or AgentRunBilled ─→ RecordCosts{run_agent, agent, Model, PieceID, 1} ─→ return / propagate
      └─→ ArtifactJobResult{workspace, model, cost_usd, output}   (no judges, gate or Ship)

## Interfaces / Contracts

```go
// ports
type RunSpec struct { WorkspacePath, Prompt string; AgentConfig json.RawMessage }
type RunResult struct { /* existing */; Model string }
type RunError struct { Result RunResult; Err error } // Error() and Unwrap()
var ErrInvalidAgentConfig = errors.New("invalid agent config")
const ModelUnknown = "unknown" // no modelUsage and no requested alias (D4)
type Checkout interface { Checkout(ctx context.Context, repo, sha, path string) error }
var ErrInvalidRepo, ErrRefNotFound, ErrCheckoutConflict error
type CostEntry struct { /* existing */; PieceID string }

// claudecode (unexported). A value that is empty or starts with '-' is rejected.
type agentConfig struct {
  Model        string   `json:"model"`
  Tools        []string `json:"tools"`
  AllowedTools []string `json:"allowed_tools"`
}

// workspace
type Overlay struct { Source string `json:"source"`; Dest string `json:"dest"` }
func ValidatePaths(roots []string, ovs []Overlay) error
func Apply(ctx context.Context, workspace string, roots []string, ovs []Overlay, beat func()) error
// ErrOutsideRoots: a destination leaves its root, lexically or through a symlink.
// ErrUnsupportedSource: a source is missing, or it is (or contains) an entry that is
// neither a regular file nor a directory — a symlink, device, socket or fifo.
var ErrOutsideRoots, ErrUnsupportedSource error

// engine
type ArtifactJobInput struct {
  JobID string `json:"job_id"`; PieceID string `json:"piece_id,omitempty"`
  Repo string `json:"repo"`; SourceRef string `json:"source_ref"`; Prompt string `json:"prompt"`
  AgentConfig json.RawMessage `json:"agent_config"` // required, non-empty
  DestinationRoots []string `json:"destination_roots"` // required, at least one
  Overlays []workspace.Overlay `json:"overlays,omitempty"`
}
type ArtifactJobResult struct { Workspace, Model string; CostUSD float64; Output string } // json: workspace, model, cost_usd, output
func runAgentAndRecord(agentCtx, ctx workflow.Context, run RunAgentInput, actor, pieceID string) (AgentResult, error)
// RunAgentInput gains AgentConfig; AgentResult gains Model (both omitempty).
// telemetry.Result gains Model, emitted as gen_ai.response.model.
```

Error mapping:

- **`validate()`**: a non-retryable `ApplicationError` of type `InvalidInput`, returned by the workflow itself before any activity is scheduled. It covers the SHA, the `job_id`, the roots, the destinations, the sources and an empty config.
- **`ErrInvalidAgentConfig`**: non-retryable, type `AgentConfig`, raised inside `RunAgent` before the process is spawned.
- **`InvalidRepo`, `RefNotFound`, `CheckoutConflict`**: non-retryable application errors of the same names.
- **`ErrOutsideRoots` and `ErrUnsupportedSource`**: non-retryable, type `OverlayRejected`.
- **`*ports.RunError`**: non-retryable `AgentRunBilled`, with the partial `AgentResult` as details.
- **Anything else**: stays retryable.

## Requirements added by design (need spec scenarios)

1. **Agent config.** An artifact job with an empty or `null` `agent_config` fails validation with `InvalidInput`. An artifact job must declare its tools and allowlist.
2. **Billed failure is recorded.** An agent run that reports a billed failure records its `run_agent` row, with model and `piece_id`, before the job fails. This applies to both job shapes. That failure is not retried automatically.
3. **Path safety.** The following are rejected: a root that is `.` or contains `.git`, an unsafe `job_id`, and symlinks inside an overlay source.
4. **No repository hooks.** The checkout runs no repository hooks, on any git invocation.
5. **Unknown model label.** When the envelope reports no `modelUsage` and no alias was requested, the row records `unknown`. The run does not fail, and a warning is logged.

## File Changes

| File | Action |
|---|---|
| `internal/ports/{agent,ledger}.go`, `internal/ports/checkout.go` | Modify / Create |
| `internal/adapters/claudecode/{config.go,runner.go}` | Create / Modify (D1–D4, D14) |
| `internal/adapters/gitcli/checkout.go` | Create |
| `internal/workspace/overlay.go` | Create |
| `internal/engine/artifact.go` | Create |
| `internal/engine/{workflow,activities}.go` | Modify |
| `internal/telemetry/instruments.go` | Modify (second cut if the change runs over budget) |
| `internal/adapters/postgres/ledger.go`, `migrations/00004_cost_entries_piece_id.sql` | Modify / Create |
| `cmd/worker/main.go` | Modify |
| `internal/engine/history_capture_test.go` | Create **first**, at base commit `9a5be05` (current HEAD) |
| `internal/engine/testdata/jobworkflow_{ship,reject}.history.json` | Create at `9a5be05`, before any production edit (`internal/engine/testdata/` does not exist yet) |
| `docs/adr/0006-artifact-jobs.md` (written), `docs/DESIGN.md` | Create / Modify |

## Affected areas and line estimate (production lines, tests excluded)

| Area | Decisions | ~Lines |
|---|---|---|
| `internal/ports/agent.go`: `AgentConfig`, `Model`, `ErrInvalidAgentConfig`, `ModelUnknown`, the `RunError` type | D1, D4, D14 | 30 |
| `internal/ports/ledger.go`: `PieceID` | D13 | 4 |
| `internal/ports/checkout.go`: the interface and three sentinels | D5 | 25 |
| `internal/adapters/claudecode/config.go`: parsing, validation, the argument builder | D1–D3 | 90 |
| `internal/adapters/claudecode/runner.go`: wiring, `modelUsage` selection, the `RunError` path | D2, D4, D14 | 85 |
| `internal/adapters/gitcli/checkout.go`: probe, worktree add, reuse checks, error classification | D5, D6 | 130 |
| `internal/workspace/overlay.go`: `ValidatePaths`, the pre-check walk, durable copy | D9–D11 | 180 |
| `internal/engine/artifact.go`: types, `validate()`, the workflow | D7–D9, D13 | 120 |
| `internal/engine/activities.go`: `CheckoutWorkspace`, `ApplyOverlay`, `withHeartbeat`, `RunAgent` mapping | D12, D14 | 85 |
| `internal/engine/workflow.go`: `runAgentAndRecord` and its two call sites | D14 | 55 |
| `internal/telemetry/instruments.go`: `Result.Model`, `gen_ai.response.model` | D4 | 8 |
| `internal/adapters/postgres/ledger.go`: the `piece_id` column | D13 | 4 |
| `migrations/00004_cost_entries_piece_id.sql` | D13 | 12 |
| `cmd/worker/main.go`: registration and wiring | — | 6 |
| **Total production** | | **~834** |

Also in the PR, outside the production count: `docs/adr/0006-artifact-jobs.md` (195 lines, written) and the `docs/DESIGN.md` update (~25 lines). Tests are excluded from all of these numbers and are expected to be larger than the production diff.

I produced this estimate independently and it comes out **~834**, above the orchestrator's ~740. The difference is three things I count and that figure may not: the SQL migration, doc comments on every exported symbol (an existing convention in this repo), and the D10 pre-check walk inside the overlay. Both numbers are far above the proposal's ~550, which never counted the D14 billed-failure path, `claudecode/config.go`, `internal/adapters/gitcli`, the `withHeartbeat` extraction, the telemetry model field or the pre-check walk. The change proceeds as one PR under the `size:exception` the user accepted.

## Testing Strategy (strict TDD)

| Layer | What | How |
|---|---|---|
| Adapter | Argument lists, table-driven: an empty config gives today's arguments; a full config gives the D2 order with `--allowedTools` last; `bypass` and `dangerously` never appear. An unknown field or a value starting with `-` is rejected and the binary never runs. Stdin is `/dev/null`. Model selection, including the empty-config envelope with no `modelUsage`, which yields `unknown` and no error. `permission_denials` with exit 0 is a success. `is_error=true` returns a `RunError` carrying cost, usage and model. A non-zero exit with a valid envelope returns a `RunError`. A non-zero exit with garbage output returns a plain error. | Fake `claude` script (`runner_test.go:17`) that records `"$@"` and `readlink /proc/self/fd/0` |
| Git adapter | A fresh detached checkout; clean reuse; conflicts (a different SHA, a dirty tree, a non-git directory); an unknown SHA; a path that is not a repo; the hook does not run. | Real `git` in `t.TempDir()`, skipped under `-short` |
| Overlay | Paths, table-driven: `..`, absolute, `.`, `.git`, the `jobsX` vs `jobs` prefix. Overwriting a tracked file keeps its mode. When the third overlay hits a symlink, **no** file is written. Source symlinks are rejected. A failure injected on the second file, then a retry, gives an identical tree and no temp files left. | Real filesystem |
| Workflow | Order: checkout → overlay → run_agent → record_cost, and nothing else runs. Invalid input yields `InvalidInput` and zero activities: a branch, a short SHA, 39 or 41 characters, a non-hex character, surrounding whitespace, a missing root, a root of `.`, a root containing `.git`, an escaping destination, an unsafe `job_id` and an empty config. The last three are `validate()`'s job, so they must fail here and not only at the overlay layer. An uppercase SHA reaches checkout lowercased. `AgentRunBilled` with details records the row, then the workflow fails. An overlay failure means `RunAgent` is never invoked. The `JobWorkflow` billed-failure path records an `agent` row and does not call `JudgeOne`. | Temporal test environment |
| Replay | `JobWorkflow` replays the ship and reject histories. They are captured at the base commit by `history_capture_test.go` (gated by `TOLLGATE_CAPTURE_HISTORY=1`), which runs on the dev server with fake activities and writes protojson. D14 does not affect them: they contain only successful `RunAgent` completions, so the error branch never runs. | `worker.NewWorkflowReplayer` |
| E2E | Real dev server; git temp repo; real overlay; real `claudecode.Runner` with a fake binary; Postgres when reachable. Asserts rows with model and `piece_id`, and no `Ship` or `JudgeOne` scheduled. A second case, where the fake returns `is_error`, asserts that the row still lands. | Modeled on `crash_resume_test.go:75-133` |
| Unchanged | `workflow_test.go` and `crash_resume_test.go` pass unedited. Their plain and `TestFailure` errors carry no `AgentRunBilled` details (`workflow_test.go:404,440`). | CI |

## Threat Matrix

| Boundary | Applicability | Response | RED tests |
|---|---|---|---|
| Documentation-like paths | N/A: no file classification or execution | — | — |
| Git repository selection | Applicable: `git -C <repo>` | The repo path must be absolute and pass `rev-parse --git-dir`; hooks are disabled; the SHA must be 40 hex characters | A relative repo is rejected; a path that is not a repo is rejected; the hook does not run |
| Commit / push state | N/A: nothing is committed or pushed | — | — |
| PR commands | N/A: no PR | — | — |
| Agent argument injection (added) | Applicable | Values starting with `-` are rejected; no shell; the multi-value flag goes last; `--restricted` ignores the repo's settings files | `-rule` and `-model` cases; the argument list includes `--restricted` |

## Migration / Rollout

Migration `00004` is nullable and has a down step. The new workflow is additive. Histories are captured before any production edit. The billed-failure fix changes how the PR shape behaves on its error path; ADR-0006 records this.

## Open Questions

- None blocking.
