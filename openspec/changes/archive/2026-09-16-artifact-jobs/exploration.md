# Exploration: artifact-jobs — a second tollgate job shape with no PR

Source: Engram `sdd/artifact-jobs/explore` (project `tollgate`, observation 1929),
produced by sdd-explore on 2026-09-16. The explore agent had no file-write tool;
the orchestrator materialized this file for the hybrid store. Load-bearing
claims (no worktree checkout, `run_agent` cost rows without Model, ADR-0002
wording) were re-verified against the code by the orchestrator.

## Current state

- `RunSpec` (`internal/ports/agent.go:8-11`) has only `WorkspacePath` and
  `Prompt`. `claudecode.Runner.Run` (`internal/adapters/claudecode/runner.go:52-59`)
  shells `claude -p <prompt> --output-format json` with no `--model`, no
  `--allowedTools` and no permission-mode flag. `CLIJudge`
  (`judge.go:19-22,33`) already carries `Model` and passes `--model`. Tests fake
  the binary as a temp shell script (`runner_test.go:14-22`).
- `JobWorkflow` (`internal/engine/workflow.go:109-230`): Prepare (117) →
  LoadRubric (124-131) → fix loop (151-229: RunAgent → N judges → DecideGate →
  Ship-or-repair). `JobInput` (37-52) has no job-kind field. `Ship` is a stub
  (`activities.go:199-203`).
- `Activities.Prepare` (`activities.go:73-83`) only runs `os.MkdirAll` on a
  per-JobID temp path. Its own comment says the git-worktree checkout "is a
  later cycle". No worktree or clone code exists anywhere in the repository.
- `CostEntry` (`internal/ports/ledger.go:8-16`): JobID, Phase, Actor, Model,
  Usage, USD, Attempt. Natural key `(job_id, phase, actor, attempt)`
  (`migrations/00002_cost_entries_natural_key.sql:5-6`; `ON CONFLICT DO NOTHING`
  at `internal/adapters/postgres/ledger.go:41`). The `run_agent` entry
  (`workflow.go:172-177`) does not set `Model`; judge entries do (200-203).
- `TaskQueue = "tollgate-jobs"` (`workflow.go:20`). The worker registers
  `engine.JobWorkflow` without a name override (`cmd/worker/main.go:61`), so the
  Temporal type is `JobWorkflow`. `JobInput` has no json tags; CLI JSON input
  uses Go field names verbatim.
- Test layers: adapter unit tests fake the CLI; `workflow_test.go` uses the
  Temporal test env with mocked activities; `crash_resume_test.go:75-133` is the
  only end-to-end test (real Temporal dev server, two workers, replay counts).
- Claude Code headless flags (docs fetched 2026-09-16:
  <https://code.claude.com/docs/en/cli-reference>,
  <https://code.claude.com/docs/en/headless>): `--model <alias>`;
  `--allowedTools "Tool" "Bash(ffmpeg:*)"` (permission-rule syntax, prefix
  match); `--permission-mode` with values `default`, `acceptEdits`, `plan`,
  `auto`, `dontAsk` and a bypass mode (default for `-p` is manual);
  `--output-format json` returns `result`, `session_id`, `total_cost_usd` and
  per-model usage, matching tollgate's `resultEnvelope`.

## Findings and contradictions

1. ADR-0002 (`0002:42-43`) says agent knobs "live in `RunSpec` as an opaque
   per-adapter config block, not as first-class fields". Adding first-class
   Model and allowlist fields contradicts it; honoring it means an opaque
   adapter config block. Either path must be explicit (ADR-0006 if superseding).
2. No job-kind branch point exists. Options: (a) `JobInput.Kind` branch inside
   `JobWorkflow`; (b) a second workflow function (duplicates ADR-0003 logic);
   (c) extract the RunAgent + fix loop + judge + gate block into a shared helper
   with two thin entry points.
3. Gate and judges for an artifact job: skip entirely (matches the approved
   decision that human gates live outside tollgate); run with an artifact rubric
   (requires reshaping `ports.JudgeRequest.Diff`, `judge.go:11-15`); or keep
   judges advisory.
4. Overlay durability (blocking review axis): the overlay needs a real worktree
   to attach to. Copy with temp name + `os.Rename` per file; hardlink or reflink
   is filesystem-dependent and risks cross-job corruption if the agent can write
   to a linked file. ADR-0001 (`0001:49-51`) requires side effects in
   activities; the overlay must be its own activity, separate from `RunAgent`,
   so a copy failure never re-bills the paid agent call. No workspace cleanup
   exists today.
5. Cost per piece (plan + preview + render = three JobIDs): no field links them.
   Options: encode the piece id in JobID (no migration, fragile parsing) or add a
   nullable `piece_id` column (additive, not a one-way door under ADR-0004's
   migration policy, `0004:55-56`); `piece_id` must not join the natural key.
   Populating `Model` on `run_agent` rows is required either way.
6. Temporal CLI submission today:
   `temporal workflow start --type JobWorkflow --task-queue tollgate-jobs --input '{"JobID":...,"Repo":...,"SourceRef":...,"Prompt":...,"RubricPath":"","JudgeModels":[...],"MaxFixAttempts":0}'`.
   New fields must fit this JSON shape.
7. The artifact-job path feeds the same ledger as PR jobs; per the dev-strategy
   scar log it needs one end-to-end test asserting cost rows land for the new
   job kind.

## Recommendation (explorer)

Option (c) for the workflow split; skip gate and judges for artifact jobs in the
first cut; build real git-worktree checkout in `Prepare` as a prerequisite; an
ADR-0006 for the new job shape, the overlay durability contract, and the
RunSpec knobs question; `piece_id` via migration; `Model` on `run_agent` rows.

## Risks (severity descending)

1. BLOCKING — no git-worktree checkout exists for any job shape. Orchestrator
   decision: scoped IN — it is the foundation of the approved goal (frozen
   engine per job) and already on tollgate's roadmap.
2. HIGH — ADR-0002 wording versus first-class RunSpec knobs.
3. HIGH — overlay-copy idempotency under at-least-once retries for large files.
4. MEDIUM — `JudgeRequest.Diff` assumes a git diff.
5. MEDIUM — no field aggregates cost across a piece's jobs.
6. LOW — no workspace cleanup policy; large-media disk growth is unbounded.
