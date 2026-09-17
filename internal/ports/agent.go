// Package ports declares the interfaces the engine depends on. Adapters
// implement them; the engine never imports an adapter (ADR-0002).
package ports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidAgentConfig is returned when AgentConfig fails to parse or
// contains a value the adapter refuses to forward to the harness (ADR-0006
// §5). It crosses the port boundary as a sentinel so RunAgent can map it to
// a non-retryable error without the engine ever inspecting adapter internals.
var ErrInvalidAgentConfig = errors.New("ports: invalid agent config")

// ModelUnknown labels a run whose resolved model could not be determined:
// no modelUsage was reported and no model alias was requested (ADR-0006 §7).
// The run does not fail; every ledger row still records SOME model label.
const ModelUnknown = "unknown"

// RunSpec describes one agent run inside a prepared workspace. AgentConfig
// is opaque to the engine (ADR-0002): it is validated and translated by the
// adapter alone.
type RunSpec struct {
	WorkspacePath string
	Prompt        string
	AgentConfig   json.RawMessage
}

// TokenUsage is the raw quantity behind a cost: what was actually consumed,
// per pricing class. Tokens are the ground truth; USD is a valuation of
// them at the price table of the moment.
type TokenUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// RunResult is the adapter-normalized outcome of one agent run. CostUSD is
// the agent-reported estimate (client-side, not authoritative billing);
// adapters whose agent reports no cost must document how they derive it.
type RunResult struct {
	CostUSD   float64
	Usage     TokenUsage
	Output    string
	SessionID string
	// Model is the resolved model id the run actually used: the highest-cost
	// modelUsage key when the harness reports one, else the requested alias,
	// else ModelUnknown (ADR-0006 §7). Output normalization, not a first-class
	// engine concept — ADR-0002 permits it.
	Model string
}

// AgentRunner runs an opaque external coding agent to completion against a
// workspace and reports the normalized outcome (ADR-0002).
type AgentRunner interface {
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}

// AgentConfigValidator is implemented by runners that can check an
// AgentConfig without starting the harness, so a job can fail on a bad
// config before any workspace is prepared. The format stays the adapter's
// (ADR-0002): the engine states what the job shape requires, and the adapter
// decides how its format meets it. A failure wraps ErrInvalidAgentConfig.
type AgentConfigValidator interface {
	ValidateConfig(cfg json.RawMessage, req AgentConfigRequirements) error
}

// AgentConfigRequirements are job-shape rules a config must satisfy on top
// of the adapter's own.
type AgentConfigRequirements struct {
	// RequireModel demands that the config pin a model: an artifact job
	// measures cost per model, so it must never run on the harness default.
	RequireModel bool
}

// ShutdownBounder is implemented by runners that can bound how long Run
// takes to return once its context is done: killing the harness, waiting
// for its output, and reading it. The engine stops the agent at least that
// long before the activity deadline, so a killed run is reported while the
// attempt is still live (ADR-0006 §8).
type ShutdownBounder interface {
	ShutdownBound() time.Duration
}

// RunError wraps a partial RunResult with the error that ended the run. It
// signals a BILLED failure: the harness produced a parseable result envelope
// (is_error=true, or a failed exit after printing one), so the partial
// cost/usage/model must be recorded before the job fails (ADR-0006 §8).
type RunError struct {
	Result RunResult
	Err    error
}

func (e *RunError) Error() string {
	return fmt.Sprintf("billed agent run failed: %v", e.Err)
}

func (e *RunError) Unwrap() error {
	return e.Err
}

// ErrAmbiguousEnvelope means the harness output held more than one result
// envelope. Anything that inherited the harness's stdout can print one, so
// the real cost cannot be told from a forged one; the run is treated like
// an unmetered one (wrapped in an UnmeteredRunError).
var ErrAmbiguousEnvelope = errors.New("ports: more than one result envelope")

// UnmeteredRunError signals a run that was killed — by a deadline, a
// cancellation, or a signal — before it printed a result envelope. The
// harness may already have spent money that nobody can measure, so the run
// must never be retried automatically: a retry would bill again on top of
// an unknown amount (ADR-0006 §8). Any other failure without an envelope
// (the binary is missing, or exited non-zero with no envelope) is a plain
// error.
type UnmeteredRunError struct {
	Err error
}

func (e *UnmeteredRunError) Error() string {
	return fmt.Sprintf("unmetered agent run (killed before reporting its cost): %v", e.Err)
}

func (e *UnmeteredRunError) Unwrap() error {
	return e.Err
}
