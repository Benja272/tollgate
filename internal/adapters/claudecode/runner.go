// Package claudecode adapts Claude Code headless (`claude -p
// --output-format json`) to the ports.AgentRunner interface (ADR-0002).
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"

	"github.com/Benja272/tollgate/internal/ports"
)

// Runner shells the Claude Code CLI. Bin is the binary to invoke, normally
// "claude"; tests point it at a fake.
type Runner struct {
	Bin string
}

var _ ports.AgentRunner = (*Runner)(nil)

// resultEnvelope is the subset of Claude Code's JSON result output tollgate
// consumes. total_cost_usd is a client-side estimate, present under both
// subscription and API-key auth.
type resultEnvelope struct {
	IsError      bool                       `json:"is_error"`
	TotalCostUSD float64                    `json:"total_cost_usd"`
	Result       string                     `json:"result"`
	SessionID    string                     `json:"session_id"`
	Usage        envelopeUsage              `json:"usage"`
	ModelUsage   map[string]modelUsageEntry `json:"modelUsage"`
}

// modelUsageEntry is one model's contribution to a run, as Claude Code
// reports it. CostUSD is what ADR-0006 D4's resolution picks the winner by:
// the modelUsage key with the highest cost is the model that actually did
// the work (a run may consult a cheaper model first and then hand off).
type modelUsageEntry struct {
	CostUSD float64 `json:"costUSD"`
}

// envelopeUsage carries the token classes the CLI reports. Cache tokens
// matter: a 10-token prompt ships ~40k tokens of session context, and
// without them the reported cost is inexplicable.
type envelopeUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

func (u envelopeUsage) toPort() ports.TokenUsage {
	return ports.TokenUsage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheCreationTokens,
	}
}

func (r *Runner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	args, err := buildArgs(spec.Prompt, spec.AgentConfig)
	if err != nil {
		return ports.RunResult{}, err
	}
	// buildArgs already validated spec.AgentConfig; the alias is only
	// needed here as the model-resolution fallback (ADR-0006 D4), so any
	// error is unreachable and safely ignored.
	cfg, _, _ := parseAgentConfig(spec.AgentConfig)

	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = spec.WorkspacePath
	// cmd.Stdin is deliberately left nil: os/exec connects a nil Stdin to
	// the null device, and an explicit *os.File open here could shadow
	// that default. Claude Code headless waits ~3s on a stdin that never
	// closes, so this must never regress (D2).

	out, runErr := cmd.Output()
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return ports.RunResult{}, fmt.Errorf("claude code run: %w", runErr)
		}
		var env resultEnvelope
		if jsonErr := json.Unmarshal(out, &env); jsonErr != nil {
			// No parseable envelope: a crash or a kill. Nothing was
			// billed, so nothing to record — stays a plain, retryable
			// error (ADR-0006 D14).
			return ports.RunResult{}, fmt.Errorf("claude code run: %w", runErr)
		}
		return ports.RunResult{}, &ports.RunError{
			Result: envelopeToResult(env, cfg.Model),
			Err:    fmt.Errorf("claude code exited non-zero: %w", runErr),
		}
	}

	var env resultEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return ports.RunResult{}, fmt.Errorf("parse claude code output: %w", err)
	}
	result := envelopeToResult(env, cfg.Model)
	if env.IsError {
		return ports.RunResult{}, &ports.RunError{
			Result: result,
			Err:    fmt.Errorf("claude code reported error: %s", env.Result),
		}
	}
	return result, nil
}

// envelopeToResult normalizes one parsed envelope into a RunResult,
// resolving the model that actually ran (ADR-0006 D4).
func envelopeToResult(env resultEnvelope, requestedAlias string) ports.RunResult {
	return ports.RunResult{
		CostUSD:   env.TotalCostUSD,
		Usage:     env.Usage.toPort(),
		Output:    env.Result,
		SessionID: env.SessionID,
		Model:     resolveModel(env.ModelUsage, requestedAlias),
	}
}

// resolveModel picks the model that actually ran: the modelUsage key with
// the highest reported cost, ties broken by the smallest id; the requested
// alias when no modelUsage was reported at all; ports.ModelUnknown when
// neither is available (ADR-0006 D4). ids are sorted before comparison so
// the result never depends on Go's randomized map iteration order.
func resolveModel(usage map[string]modelUsageEntry, requestedAlias string) string {
	if len(usage) == 0 {
		if requestedAlias != "" {
			return requestedAlias
		}
		return ports.ModelUnknown
	}

	ids := make([]string, 0, len(usage))
	for id := range usage {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	best := ids[0]
	bestCost := usage[best].CostUSD
	for _, id := range ids[1:] {
		if usage[id].CostUSD > bestCost {
			best = id
			bestCost = usage[id].CostUSD
		}
	}
	return best
}
