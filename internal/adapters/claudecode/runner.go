// Package claudecode adapts Claude Code headless (`claude -p
// --output-format json`) to the ports.AgentRunner interface (ADR-0002).
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

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
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
}

func (r *Runner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	cmd := exec.CommandContext(ctx, r.Bin, "-p", spec.Prompt, "--output-format", "json")
	cmd.Dir = spec.WorkspacePath

	out, err := cmd.Output()
	if err != nil {
		return ports.RunResult{}, fmt.Errorf("claude code run: %w", err)
	}

	var env resultEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return ports.RunResult{}, fmt.Errorf("parse claude code output: %w", err)
	}
	if env.IsError {
		return ports.RunResult{}, fmt.Errorf("claude code reported error: %s", env.Result)
	}

	return ports.RunResult{
		CostUSD:   env.TotalCostUSD,
		Output:    env.Result,
		SessionID: env.SessionID,
	}, nil
}
