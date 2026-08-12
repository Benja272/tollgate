// Package ports declares the interfaces the engine depends on. Adapters
// implement them; the engine never imports an adapter (ADR-0002).
package ports

import "context"

// RunSpec describes one agent run inside a prepared workspace.
type RunSpec struct {
	WorkspacePath string
	Prompt        string
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
}

// AgentRunner runs an opaque external coding agent to completion against a
// workspace and reports the normalized outcome (ADR-0002).
type AgentRunner interface {
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}
