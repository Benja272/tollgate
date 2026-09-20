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

### Requirement: Empty Config Compatibility

With an empty `AgentConfig`, the adapter MUST build exactly the command it built before this change.

#### Scenario: Empty config matches today's PR-shape command

- GIVEN an empty `AgentConfig`
- WHEN the adapter builds the CLI command
- THEN the resulting arguments are identical to those built before this change
