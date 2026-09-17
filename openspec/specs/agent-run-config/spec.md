# Agent Run Config Specification

## Purpose

Defines the per-adapter agent configuration (model, tool allowlist) and its translation into headless CLI flags, without ever enabling a bypass mode.

## Requirements

### Requirement: Model and Allowlist Reach the CLI

The adapter MUST translate the requested model and tool allowlist from `AgentConfig` into the headless CLI invocation.

#### Scenario: Configured model and allowlist are passed

- GIVEN an `AgentConfig` naming a model and a tool allowlist
- WHEN the adapter builds the CLI command
- THEN the command includes that model and that allowlist

### Requirement: No Bypass Permission Mode

The adapter MUST NOT pass any bypass or skip-permissions flag, regardless of `AgentConfig` contents.

#### Scenario: Bypass flag never present

- GIVEN any valid `AgentConfig`, including one requesting broad tool access
- WHEN the adapter builds the CLI command
- THEN no bypass or skip-permissions flag is present in the command

### Requirement: Prompt Is Never Parsed as a Flag

The adapter MUST pass the prompt as the single operand after a `--` terminator placed after every flag, for every `AgentConfig`, including an empty one. A prompt starting with `-` is legitimate and MUST NOT be rejected. *(Added by the post-archive review, B5.)*

#### Scenario: Dash-leading prompt stays an operand

- GIVEN a prompt such as `--dangerously-skip-permissions` or a markdown list starting with `- `
- WHEN the adapter builds the CLI command, with an empty or a non-empty `AgentConfig`
- THEN the last two arguments are `--` and the prompt, and no argument before `--` is the prompt

### Requirement: Config Values Are Validated, Never Rewritten

The adapter MUST reject, before starting any process, an `AgentConfig` that has unknown fields or trailing data; a model with a leading `-`, whitespace or control characters; a `tools` entry that is empty, starts with `-`, or contains a comma, whitespace or a control character; or an `allowed_tools` entry that is empty, starts with `-`, contains a control character, has unbalanced parentheses, or has a comma or whitespace outside parentheses. `"tools": []` MUST produce `--tools ""` (no tools); an absent `tools` key MUST NOT emit `--tools`. *(Added by the post-archive review.)*

#### Scenario: A separator cannot smuggle in a second rule

- GIVEN an `allowed_tools` entry `Read,Bash` or `Read Bash`
- WHEN the adapter validates the config
- THEN it fails with an invalid-config error and no process starts

### Requirement: Empty Config Compatibility

With an empty `AgentConfig`, the adapter MUST build the minimal command: the same flags the adapter built before the artifact-jobs change, with the prompt moved behind the `--` terminator. *(Amended by the post-archive review: the pre-change argv, `-p <prompt> --output-format json`, let a dash-leading prompt parse as a flag.)*

#### Scenario: Empty config builds the minimal command

- GIVEN an empty `AgentConfig`
- WHEN the adapter builds the CLI command
- THEN the arguments are exactly `-p --output-format json -- <prompt>`
