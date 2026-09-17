// Package claudecode adapts Claude Code headless (`claude -p
// --output-format json`) to the ports.AgentRunner interface (ADR-0002).
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"github.com/Benja272/tollgate/internal/ports"
)

// defaultWaitDelay bounds how long Run waits for the agent's output pipes
// after the agent exits or is killed. A background process the agent
// started inherits stdout and would otherwise hold Run open until the
// activity times out, losing an envelope that was already printed.
const defaultWaitDelay = 10 * time.Second

// stderrTailBytes bounds the stderr excerpt carried in run errors.
const stderrTailBytes = 2048

// Runner shells the Claude Code CLI. Bin is the binary to invoke, normally
// "claude"; tests point it at a fake. WaitDelay overrides defaultWaitDelay.
type Runner struct {
	Bin       string
	WaitDelay time.Duration
}

var (
	_ ports.AgentRunner          = (*Runner)(nil)
	_ ports.AgentConfigValidator = (*Runner)(nil)
)

// resultEnvelope is the subset of Claude Code's JSON result output tollgate
// consumes. total_cost_usd is a client-side estimate, present under both
// subscription and API-key auth.
type resultEnvelope struct {
	Type         string                     `json:"type"`
	IsError      bool                       `json:"is_error"`
	TotalCostUSD *float64                   `json:"total_cost_usd"`
	Result       string                     `json:"result"`
	SessionID    string                     `json:"session_id"`
	Usage        envelopeUsage              `json:"usage"`
	ModelUsage   map[string]modelUsageEntry `json:"modelUsage"`
}

func (e resultEnvelope) cost() float64 {
	if e.TotalCostUSD == nil {
		return 0
	}
	return *e.TotalCostUSD
}

// errNoEnvelope means the output holds no result envelope.
var errNoEnvelope = errors.New("no result envelope in claude code output")

// parseEnvelope finds the result envelope in the CLI's stdout: the whole
// output when it is one, else the last line that is one, so warnings the
// CLI prints around it do not hide a billed run. A value counts only when
// its type is "result" and it reports total_cost_usd: `null`, `{}` and
// other JSON objects are not envelopes.
func parseEnvelope(out []byte) (resultEnvelope, error) {
	if env, ok := decodeEnvelope(bytes.TrimSpace(out)); ok {
		return env, nil
	}
	lines := bytes.Split(out, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		if env, ok := decodeEnvelope(line); ok {
			return env, nil
		}
	}
	return resultEnvelope{}, errNoEnvelope
}

func decodeEnvelope(b []byte) (resultEnvelope, bool) {
	var env resultEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return resultEnvelope{}, false
	}
	return env, env.Type == "result" && env.TotalCostUSD != nil
}

// modelUsageEntry is one model's contribution to a run, as Claude Code
// reports it. CostUSD is what ADR-0006 §7's resolution picks the winner by:
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

// ValidateConfig checks cfg exactly as Run would, without starting the CLI,
// and reports the model it requests.
func (r *Runner) ValidateConfig(cfg json.RawMessage) (string, error) {
	c, _, err := parseAgentConfig(cfg)
	if err != nil {
		return "", err
	}
	return c.Model, nil
}

func (r *Runner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	args, err := buildArgs(spec.Prompt, spec.AgentConfig)
	if err != nil {
		return ports.RunResult{}, err
	}
	// buildArgs already validated spec.AgentConfig; the alias is only
	// needed here as the model-resolution fallback (ADR-0006 §7), so the
	// error is unreachable and safely ignored.
	cfg, _, _ := parseAgentConfig(spec.AgentConfig)

	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = spec.WorkspacePath
	// cmd.Stdin is deliberately left nil: os/exec connects a nil Stdin to
	// the null device, and an explicit *os.File open here could shadow
	// that default. Claude Code headless waits ~3s on a stdin that never
	// closes, so this must never regress.
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: stderrTailBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	// The agent and everything its tools start share one process group,
	// so a cancellation kills all of them, not only the CLI.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = r.waitDelay()

	runErr := cmd.Run()
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// The CLI exited cleanly but something it started still holds its
		// output: kill the leftovers (the group is still populated, so its
		// id cannot have been reused) and read what the CLI printed.
		_ = killGroup(cmd)
		runErr = nil
	}

	env, envErr := parseEnvelope(stdout.Bytes())
	if runErr != nil {
		if envErr == nil {
			// The CLI reported its cost before failing: the run was billed.
			return ports.RunResult{}, &ports.RunError{
				Result: envelopeToResult(env, cfg.Model),
				Err:    fmt.Errorf("claude code exited with a failure: %w%s", runErr, stderr.suffix()),
			}
		}
		if ctx.Err() != nil || killedBySignal(runErr) {
			// Killed mid-run: the harness may have spent money that no
			// envelope reports.
			cause := runErr
			if ctx.Err() != nil {
				cause = fmt.Errorf("%w (%w)", runErr, ctx.Err())
			}
			return ports.RunResult{}, &ports.UnmeteredRunError{
				Err: fmt.Errorf("claude code run: %w%s", cause, stderr.suffix()),
			}
		}
		return ports.RunResult{}, fmt.Errorf("claude code run: %w%s", runErr, stderr.suffix())
	}

	if envErr != nil {
		return ports.RunResult{}, fmt.Errorf("parse claude code output: %w%s", envErr, stderr.suffix())
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

func (r *Runner) waitDelay() time.Duration {
	if r.WaitDelay > 0 {
		return r.WaitDelay
	}
	return defaultWaitDelay
}

// killGroup SIGKILLs the command's whole process group.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func killedBySignal(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
}

// tailBuffer keeps only the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

// suffix renders the tail for an error message, empty when there is none.
func (b *tailBuffer) suffix() string {
	tail := bytes.TrimSpace(b.buf)
	if len(tail) == 0 {
		return ""
	}
	return fmt.Sprintf("; stderr tail: %q", tail)
}

// envelopeToResult normalizes one parsed envelope into a RunResult,
// resolving the model that actually ran (ADR-0006 §7).
func envelopeToResult(env resultEnvelope, requestedAlias string) ports.RunResult {
	return ports.RunResult{
		CostUSD:   env.cost(),
		Usage:     env.Usage.toPort(),
		Output:    env.Result,
		SessionID: env.SessionID,
		Model:     resolveModel(env.ModelUsage, requestedAlias),
	}
}

// resolveModel picks the model that actually ran: the modelUsage key with
// the highest reported cost, ties broken by the smallest id; the requested
// alias when no modelUsage was reported at all; ports.ModelUnknown when
// neither is available (ADR-0006 §7). ids are sorted before comparison so
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
