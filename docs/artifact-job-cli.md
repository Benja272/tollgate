# Submitting an artifact job

`ArtifactJobWorkflow` (ADR-0006) is started like any other Temporal
workflow, via the `temporal` CLI against the worker's task queue
(`engine.TaskQueue`, `"tollgate-jobs"`).

`ArtifactJobInput` carries no JSON tags, so the payload's field names are
the Go struct's field names verbatim — including nested `Overlay{Source,
Dest}` entries:

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
    "Overlays": [{"Source": "/abs/path/prepared/plan.md", "Dest": "output/piece-42/plan.md"}],
    "AgentTimeoutMinutes": 60
  }'
```

Field notes (see `openspec/specs/artifact-job/spec.md` "Input Validation"
for the full MUSTs):

- `SourceRef` must be exactly 40 lowercase-or-uppercase hex characters (a
  full commit SHA, no branch/tag/short SHA); it is normalized to lowercase
  before checkout, but is never trimmed — surrounding whitespace is rejected.
- `JobID` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`: it becomes the
  literal path segment `tollgate-artifact-<JobID>` under the worker's
  configured workspace root.
- `AgentConfig` must be present, must parse under the adapter's rules, and
  must name a `model`. It is checked by the first activity, before any
  checkout; a missing field, `null`, or an empty string fails validation
  before any activity runs.
- `Prompt` must not be blank; `PieceID` may be omitted but not blank.
- `AgentTimeoutMinutes` bounds the agent run. `0` (or omitted) means 60
  minutes, which is sized for a render; the maximum is 1440. A run cut off
  by this timeout is unmetered: it is logged, counted, and never retried.
- `DestinationRoots` must contain at least one entry; every `Overlays[].Dest`
  must resolve inside one of them.
- `Repo` must be an absolute local clone path — the git adapter never clones.
- The commit at `SourceRef` must not track any path with a segment ending
  in `.tollgate.tmp`, which is reserved for overlay temp files.

The worker places workspaces under `TOLLGATE_WORKSPACE_ROOT` (an absolute
path; default: the OS temp directory) and refuses to start with a relative
one.

**Verified against a live dev server** (2026-09-16, `temporal server
start-dev`): the exact payload above (with a real repo/SHA/overlay source
substituted) was submitted via this exact `temporal workflow start` command
against a worker running `ArtifactJobWorkflow` with a fake `claude` binary,
and completed with status `COMPLETED`; the overlay file landed at
`<WorkspaceRoot>/tollgate-artifact-piece-42-plan/output/piece-42/plan.md`
with the exact content of the source file. This is not a hand-written,
untested shape.
