# Workspace Preparation Specification

## Purpose

Defines the pinned git-worktree checkout used by artifact jobs and the durable, root-bounded overlay that places edited files into it.

## Requirements

### Requirement: Pinned Worktree Checkout

For an artifact job, the workspace MUST be a git worktree of `Repo` checked out at exactly `SourceRef`, detached. Checkout MUST run with repository hooks disabled.

#### Scenario: Fresh checkout

- GIVEN no existing worktree at the job's workspace path
- WHEN checkout runs
- THEN a detached worktree at exactly `SourceRef` is created at that path

#### Scenario: Retry reuses a matching worktree

- GIVEN an existing worktree at the workspace path already at `SourceRef`
- WHEN checkout is retried
- THEN it succeeds by reusing the existing worktree, without re-cloning

#### Scenario: Conflicting path fails non-retryably

- GIVEN an existing path at the workspace location that is not a worktree at `SourceRef`
- WHEN checkout runs
- THEN it fails non-retryably

#### Scenario: Checkout runs no repository hooks

- GIVEN a repository whose hooks would, if run, modify the workspace or its environment
- WHEN checkout runs
- THEN no repository hook executes

### Requirement: Durable, Root-Bounded Overlay

The overlay MUST place files atomically, inside the declared destination roots only, as its own activity. The overlay MUST reject any symlink found anywhere inside an overlay source before writing any file.

#### Scenario: Retry leaves no partial file

- GIVEN an overlay activity that failed partway through copying a file
- WHEN the activity is retried
- THEN the destination is either the prior complete content or the fully new content, never partial

#### Scenario: Overwrite of a tracked file inside a root is allowed

- GIVEN a destination path git tracks at the pinned commit, inside a declared root
- WHEN the overlay writes to it
- THEN the write succeeds and replaces the tracked content

#### Scenario: Symlink escape rejected

- GIVEN an overlay destination that resolves, via a symlink, outside every declared root
- WHEN the overlay activity runs
- THEN it fails non-retryably and no file is written

#### Scenario: Symlink inside an overlay source rejected

- GIVEN a symlink located anywhere inside an overlay source, whether or not it escapes a declared root
- WHEN the overlay activity runs
- THEN it fails non-retryably and no file is written

### Requirement: Overlay Failure Isolation

Overlay MUST run as an activity distinct from the agent run, so its failure never re-runs or re-bills the agent.

#### Scenario: Overlay failure prevents the agent run

- GIVEN an overlay that fails
- WHEN the artifact job runs
- THEN the agent is never invoked and no `run_agent` cost row exists for that job
