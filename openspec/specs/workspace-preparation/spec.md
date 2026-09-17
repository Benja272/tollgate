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

- GIVEN an existing path at the workspace location that is not the root of a clean, linked worktree of `Repo` at `SourceRef` (compared after resolving symlinks; a subdirectory of such a worktree, or the repository's main worktree, is a conflict). The path itself MUST NOT be a symlink. "Clean" excludes ignored and untracked files and any assume-unchanged or skip-worktree index entry. *(Second round: R7, R11.)*
- WHEN checkout runs
- THEN it fails non-retryably

#### Scenario: Transient checkout failures stay retryable

- GIVEN a checkout whose context is done, or a worker with no `git` binary
- WHEN checkout fails
- THEN the failure is not reported as an invalid repository, unknown ref or conflict, and stays retryable *(Second round: R9.)*

#### Scenario: Pinned tree using the reserved temp suffix refused

- GIVEN a commit that tracks a path with a segment ending in `.tollgate.tmp`
- WHEN checkout runs
- THEN it fails non-retryably before any worktree is created *(Second round: R10.)*

#### Scenario: Checkout runs no repository hooks

- GIVEN a repository whose hooks would, if run, modify the workspace or its environment
- WHEN checkout runs
- THEN no repository hook executes

### Requirement: Durable, Root-Bounded Overlay

The overlay MUST place files atomically, inside the declared destination roots only, as its own activity. The overlay MUST reject any symlink found anywhere inside an overlay source before writing any file.

*(Made more precise by the post-archive review, B1-B4.)* Before any write, the overlay MUST check every future destination path component, including each file's temporary name, and reject an existing symlink on any of them. Writes MUST NOT follow a symlink even if one appears after that check. The overlay MUST reject source entries that are not regular files or directories, that end in the temporary-file suffix, or that contain a `.git` segment in any letter case, and destination paths with either reserved name. A file overlay whose destination equals a declared root MUST be rejected. Every directory on the path to a destination MUST be durable: its parent is fsynced, whether this attempt or an earlier, failed one created it *(second round, R8)*. A destination that can never be written (a file onto a directory, a path through a file, a file/directory collision, a name too long for its temporary name, a directory at a temporary name) MUST fail non-retryably before anything is written. A leftover temporary file of any mode MUST NOT block a retry.

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

#### Scenario: Symlink below a root, tracked at the pinned commit, rejected

- GIVEN a symlink at the pinned commit below a declared root that points at files outside every root
- WHEN an overlay would write through it, or through a symlink sitting at a file's temporary name
- THEN the overlay fails non-retryably and the files outside the roots are unchanged in content and mode

#### Scenario: Reserved names in a source rejected

- GIVEN an overlay source tree containing a `.git` (any case) segment, an entry ending in the temporary-file suffix, or a FIFO
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
