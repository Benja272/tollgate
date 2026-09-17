# Cost Ledger Specification

## Purpose

Defines how `run_agent` cost rows record the model and how artifact-job spend is attributed to a piece without a status column.

## Requirements

### Requirement: Model Recorded on run_agent Rows

Every `run_agent` cost row MUST record the model that ran.

#### Scenario: Model present on a cost row

- GIVEN a completed agent run
- WHEN its cost row is inserted
- THEN the row's model field matches the model reported by the adapter

### Requirement: Natural Key Distinguishes Executions

Every cost row MUST record the Temporal run id of the execution that wrote it, and the ledger's natural key MUST be `(job_id, run_id, phase, actor, attempt)`: a retried write within one execution is idempotent, and a second execution of the same `job_id` keeps its own rows. *(Added by the post-archive review, B8.)*

#### Scenario: Same job_id executed twice

- GIVEN two executions of an artifact job with the same `job_id` and `piece_id`, billing $0.50 and $0.80
- WHEN both complete
- THEN both `run_agent` rows exist and per-piece spend is $1.30

### Requirement: Nullable piece_id Outside the Natural Key

Ledger rows MUST carry a nullable `piece_id`: set when the caller provides one, NULL otherwise, and never part of the row's natural key.

#### Scenario: piece_id recorded when given

- GIVEN an artifact job started with a `piece_id`
- WHEN its cost rows are inserted
- THEN each row's `piece_id` column equals the given value

#### Scenario: piece_id NULL when omitted

- GIVEN an artifact job started without a `piece_id`
- WHEN its cost rows are inserted
- THEN each row's `piece_id` column is NULL

### Requirement: Billed Failure Is Recorded Before the Job Fails

When an agent run returns a parseable result envelope reporting `is_error=true`, or a non-zero exit with a parseable envelope, the runner MUST record the `run_agent` cost row with the resolved model before the job fails. This applies to BOTH the artifact job and the PR-shaped `JobWorkflow`. Such a billed failure MUST NOT be retried automatically. When the process produces no parseable envelope, no cost row MUST be recorded. *(Made more precise by the post-archive review, B6/B7.)* If the run was killed (deadline, cancellation, or signal) before printing an envelope, it MAY have billed an unknown amount: the failure MUST be non-retryable, logged, and counted as an unmetered run. Any other failure with no envelope keeps its current retry behavior. A billed failure's cost, usage and model MUST also reach telemetry. If a billed failure's row cannot be recorded, the job MUST fail with an error that says the spend was not recorded.

#### Scenario: Billed failure records its cost row before failing (artifact job)

- GIVEN an agent run that returns a parseable envelope with `is_error=true`
- WHEN the artifact job processes the result
- THEN a `run_agent` cost row with the resolved model is recorded, the job then fails, and the failure is not retried automatically

#### Scenario: Billed failure records its cost row before failing (PR-shaped JobWorkflow)

- GIVEN an agent run in the PR-shaped `JobWorkflow` that returns a parseable envelope with a non-zero exit code
- WHEN the workflow processes the result
- THEN a `run_agent` cost row with the resolved model is recorded, the job then fails, and the failure is not retried automatically

#### Scenario: Killed run records nothing and is not retried

- GIVEN an agent run killed by its deadline or a signal before printing a result envelope
- WHEN the job processes the failure
- THEN no cost row is recorded, the unmetered run is logged and counted, and the failure is not retried automatically

#### Scenario: Other unparseable failure records nothing and stays retryable

- GIVEN an agent run that exited non-zero on its own without printing a result envelope
- WHEN the job processes the failure
- THEN no cost row is recorded, and the failure keeps its current retry behavior

### Requirement: Per-Piece Spend Aggregation

Per-piece total spend MUST be the sum of all cost rows across the piece's jobs and their executions, regardless of each job's outcome.

#### Scenario: Total includes a failed job's spend

- GIVEN a piece with three jobs sharing one `piece_id`, one of which failed after the agent ran
- WHEN per-piece spend is computed
- THEN the total includes the failed job's recorded cost rows
