// Package ports declares the interfaces the engine depends on. Adapters
// implement them; the engine never imports an adapter (ADR-0002).
package ports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidAgentConfig is returned when AgentConfig fails to parse or
// contains a value the adapter refuses to forward to the harness (ADR-0006
// D1). It crosses the port boundary as a sentinel so RunAgent can map it to
// a non-retryable error without the engine ever inspecting adapter internals.
var ErrInvalidAgentConfig = errors.New("ports: invalid agent config")

// ModelUnknown labels a run whose resolved model could not be determined:
// no modelUsage was reported and no model alias was requested (ADR-0006 D4).
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
	// else ModelUnknown (ADR-0006 D4). Output normalization, not a first-class
	// engine concept — ADR-0002 permits it.
	Model string
}

// AgentRunner runs an opaque external coding agent to completion against a
// workspace and reports the normalized outcome (ADR-0002).
type AgentRunner interface {
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}

// RunError wraps a partial RunResult with the error that ended the run. It
// signals a BILLED failure: the harness produced a parseable result envelope
// (is_error=true, or a non-zero exit with a parseable envelope) before
// failing, so the partial cost/usage/model must be recorded before the job
// fails (ADR-0006 D14). A crash or kill with no parseable envelope is a
// plain error instead — nothing was billed, so nothing to record.
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
