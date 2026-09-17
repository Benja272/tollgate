# Artifact Job Specification

## Purpose

Defines the no-PR job shape that runs one headless agent turn over a pinned workspace and records its cost, with no judges, no gate, and no PR.

## Requirements

### Requirement: Input Validation

The workflow MUST validate `SourceRef`, `agent_config`, `job_id`, and destination roots before any paid call, failing non-retryably on violation. `SourceRef` MUST be exactly 40 hexadecimal characters; both uppercase and lowercase hex digits are accepted and the value MUST be normalized to lowercase before use. Surrounding whitespace MUST be rejected, not trimmed. `agent_config` MUST be present and non-empty; an empty or `null` value fails validation. A destination root MUST NOT equal `.` after path cleaning and MUST NOT contain a `.git` path segment. `job_id` MUST match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`; any value that does not match is unsafe and MUST be rejected.

*(Added by the post-archive review.)* `Repo` MUST be an absolute path. `Prompt` MUST NOT be blank. `piece_id` MAY be omitted but MUST NOT be blank. `agent_timeout_minutes` MUST be between 0 and 1440, checked on the integer value before any conversion (second round, R2); 0 means the 60-minute default. The adapter MUST parse `agent_config` and MUST enforce the artifact job's requirement that it name a model; the engine passes the requirement and never parses the config. This check is the job's first activity and MUST fail non-retryably before any checkout or overlay. Workflow code cannot parse the adapter's config format without breaking ADR-0002.

#### Scenario: Non-SHA source ref rejected

- GIVEN a `SourceRef` that is a branch, a tag, or a short SHA
- WHEN the artifact job starts
- THEN it fails non-retryably before checkout or agent invocation

#### Scenario: Valid 40-character hex SourceRef passes validation

- GIVEN a `SourceRef` of exactly 40 hexadecimal characters
- WHEN the artifact job starts
- THEN input validation passes (the commit may still not exist, which is a later checkout failure, not a validation failure)

#### Scenario: 39-character SourceRef rejected

- GIVEN a `SourceRef` of exactly 39 hexadecimal characters
- WHEN the artifact job starts
- THEN it fails non-retryably before any paid call

#### Scenario: 41-character SourceRef rejected

- GIVEN a `SourceRef` of exactly 41 hexadecimal characters
- WHEN the artifact job starts
- THEN it fails non-retryably before any paid call

#### Scenario: 40-character SourceRef with a non-hex character rejected

- GIVEN a `SourceRef` of exactly 40 characters where at least one character is not a hexadecimal digit
- WHEN the artifact job starts
- THEN it fails non-retryably before any paid call

#### Scenario: Destination root missing or unsafe

- GIVEN a job input with no destination roots, or a root that is absolute or escapes the workspace
- WHEN the artifact job starts
- THEN it fails non-retryably before any paid call

#### Scenario: Overlay destination escapes declared roots

- GIVEN an overlay destination that resolves outside every declared root, including via `..` segments
- WHEN input validation runs
- THEN the job fails non-retryably before any paid call

#### Scenario: Destination root of "." rejected

- GIVEN a destination root that, after path cleaning, equals `.`
- WHEN the artifact job starts
- THEN it fails non-retryably before any paid call

#### Scenario: Destination root containing a .git segment rejected

- GIVEN a destination root that contains a `.git` path segment
- WHEN the artifact job starts
- THEN it fails non-retryably before any paid call

#### Scenario: Empty or null agent_config rejected

- GIVEN a job input whose `agent_config` is empty or `null`
- WHEN the artifact job starts
- THEN it fails non-retryably with an invalid-input error before any activity runs

#### Scenario: Unparseable or model-less agent_config rejected before checkout

- GIVEN an `agent_config` the adapter rejects, or one that names no model
- WHEN the artifact job starts
- THEN it fails non-retryably, and no checkout, overlay or agent run happens

#### Scenario: Unsafe job_id rejected

- GIVEN a `job_id` that does not match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`
- WHEN the artifact job starts
- THEN it fails non-retryably before any paid call

### Requirement: Permanent Failures Are Not Retried

Checkout conflicts, invalid repositories, unresolvable refs, overlay boundary and source violations, and invalid agent configs MUST reach Temporal as non-retryable errors, and MUST be attempted once. The agent run's timeout for an artifact job MUST be configurable. The agent MUST be stopped before the activity deadline by a margin that covers the runner's whole shutdown path, so that a timed-out run is reported as unmetered rather than retried by the server. *(Added by the post-archive review, B6/B10.)*

#### Scenario: Checkout conflict attempted once

- GIVEN a workspace path that conflicts with the requested checkout
- WHEN the artifact job runs
- THEN the checkout activity runs exactly once and the job fails non-retryably

### Requirement: No PR, No Gate

An artifact job MUST NOT invoke `Ship`, run judges, or evaluate a gate.

#### Scenario: Job completes without a PR

- GIVEN a valid artifact job input
- WHEN the workflow completes
- THEN no `Ship` call occurs and no judge or gate activity runs

#### Scenario: End-to-end run on a live Temporal server

- GIVEN a real Temporal dev server and a valid full-SHA input
- WHEN the artifact job starts and runs to completion
- THEN `run_agent` cost rows are recorded with the model, and no PR is opened

### Requirement: Coexistence with the PR-Shaped JobWorkflow

Adding `ArtifactJobWorkflow` MUST NOT change `JobWorkflow` behavior, EXCEPT for these changes, none of which adds, removes or reorders a step. The post-archive review added the run id on every cost row, the non-retryable classification of unmetered runs and invalid configs, and the prompt terminator in the agent's argv. The original change made two recorded-data changes: (a) the billed-failure cost-accounting fix (see Cost Ledger: Billed Failure Is Recorded Before the Job Fails), which applies to `JobWorkflow` as well as to artifact jobs; and (b) on the success path, its `run_agent` cost rows now record the model and its `invoke_agent` spans now carry `gen_ai.response.model`.

#### Scenario: Existing PR-shape tests pass unchanged

- GIVEN the existing `workflow_test.go` and `crash_resume_test.go` suites
- WHEN they run after this change
- THEN they pass without any edited assertions, except that the expected cost rows now carry the run id (post-archive review, B8)

#### Scenario: Pre-change history replays cleanly

- GIVEN a `JobWorkflow` history recorded before this change
- WHEN it is replayed with `WorkflowReplayer` after this change
- THEN the replay succeeds with no non-determinism error

### Requirement: CLI Submission

An artifact job MUST be startable via `temporal workflow start` with a documented JSON input shape.

#### Scenario: Start via CLI

- GIVEN a JSON input matching the documented artifact-job schema
- WHEN an operator runs `temporal workflow start` with it
- THEN the workflow accepts the input and begins execution
