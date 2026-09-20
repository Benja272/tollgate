// Package claudecode adapts Claude Code headless (`claude -p
// --output-format json`) to the ports.AgentRunner interface (ADR-0002).
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Benja272/tollgate/internal/ports"
)

// defaultWaitDelay bounds how long Run waits for the agent's output pipes
// after the agent exits or is killed. A process that escaped the agent's
// process group can hold stdout open; without a bound it would keep Run
// open until the activity times out, losing an envelope already printed.
const defaultWaitDelay = 10 * time.Second

// stderrTailBytes bounds the stderr excerpt carried in run errors.
const stderrTailBytes = 2048

// defaultStdoutCap bounds the captured stdout. A result envelope is small;
// output beyond this means something other than the CLI is writing, and the
// run's cost can no longer be trusted.
const defaultStdoutCap = 64 << 20

// maxErrorResultBytes bounds how much of the harness's own reported result
// text travels inside a run error's message. The cap is far below the
// stdout cap on purpose: a billed failure exists to carry the COST, and its
// message crosses process boundaries where a multi-megabyte string is
// rejected outright (ADR-0006 §8).
const maxErrorResultBytes = 2048

// agentGroupRecordSuffix names the file that records the process group of
// the agent currently running in a workspace.
const agentGroupRecordSuffix = ".tollgate-agent-group"

// agentGroupRecordPath is where a workspace's agent process group is
// recorded: BESIDE the workspace, never inside it. The agent is untrusted
// (ADR-0006) and must not be able to plant a process group tollgate would
// kill. An empty workspace path means no record is kept.
func agentGroupRecordPath(workspace string) string {
	if workspace == "" {
		return ""
	}
	return filepath.Clean(workspace) + agentGroupRecordSuffix
}

// killRecordedGroup kills the process group an earlier run recorded for
// this workspace and clears the record. A recorded group whose leader is
// gone is normal: the record outlives only what escaped it. The pid may
// also have been reused by then, so the kill is best-effort by design —
// ADR-0006 §8 records that residual.
func killRecordedGroup(workspace string) {
	path := agentGroupRecordPath(workspace)
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		// A pgid of 0 or 1 would signal this process's own group, or every
		// process this user owns; neither can be an agent tollgate started.
		if pgid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pgid > 1 {
			_ = killProcessGroup(pgid, syscall.SIGKILL)
		}
	}
	_ = os.Remove(path)
}

// recordAgentGroup records the group of a run that has just started, so a
// worker death between here and the run's return leaves the next run
// something to kill. Setpgid makes the leader's pid its group id.
func recordAgentGroup(workspace string, pgid int) {
	if path := agentGroupRecordPath(workspace); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(pgid)+"\n"), 0o600)
	}
}

// clearAgentGroupRecord drops the record of a run that has returned: its
// group has already been killed.
func clearAgentGroupRecord(workspace string) {
	if path := agentGroupRecordPath(workspace); path != "" {
		_ = os.Remove(path)
	}
}

// Runner shells the Claude Code CLI. Bin is the binary to invoke, normally
// "claude"; tests point it at a fake. WaitDelay overrides defaultWaitDelay.
type Runner struct {
	Bin       string
	WaitDelay time.Duration

	// stdoutCap overrides defaultStdoutCap; tests shrink it.
	stdoutCap int
	// cancelSignal is sent to the process group on cancellation; zero means
	// SIGKILL. Tests use a catchable signal.
	cancelSignal syscall.Signal
}

var (
	_ ports.AgentRunner          = (*Runner)(nil)
	_ ports.AgentConfigValidator = (*Runner)(nil)
	_ ports.ShutdownBounder      = (*Runner)(nil)
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

// parseEnvelope finds the single usable result envelope in the CLI's
// stdout. Anything that inherited stdout can print a line, so the output
// must hold EXACTLY ONE result-typed JSON object — the whole output, or one
// line among noise lines. More than one is ErrAmbiguousEnvelope: the real
// envelope cannot be told from a forged one. One without total_cost_usd,
// or none, is errNoEnvelope.
func parseEnvelope(out []byte) (resultEnvelope, error) {
	var candidates [][]byte
	if whole := bytes.TrimSpace(out); isResultObject(whole) {
		candidates = [][]byte{whole}
	} else {
		for _, line := range bytes.Split(out, []byte("\n")) {
			if line = bytes.TrimSpace(line); isResultObject(line) {
				candidates = append(candidates, line)
			}
		}
	}
	switch len(candidates) {
	case 0:
		return resultEnvelope{}, errNoEnvelope
	case 1:
	default:
		return resultEnvelope{}, fmt.Errorf("%d result envelopes in stdout: %w", len(candidates), ports.ErrAmbiguousEnvelope)
	}
	var env resultEnvelope
	if err := json.Unmarshal(candidates[0], &env); err != nil || env.TotalCostUSD == nil {
		return resultEnvelope{}, fmt.Errorf("%w: the result envelope reports no usable total_cost_usd", errNoEnvelope)
	}
	return env, nil
}

// isResultObject reports whether b is a JSON object whose type is "result".
func isResultObject(b []byte) bool {
	if len(b) == 0 || b[0] != '{' {
		return false
	}
	var probe struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(b, &probe) == nil && probe.Type == "result"
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
// plus the job shape's requirements.
func (r *Runner) ValidateConfig(cfg json.RawMessage, req ports.AgentConfigRequirements) error {
	c, _, err := parseAgentConfig(cfg)
	if err != nil {
		return err
	}
	if req.RequireModel && c.Model == "" {
		return invalidConfig("this job requires a model")
	}
	return nil
}

// ShutdownBound is how long Run can take after its context is done: the
// group kill is immediate, then Run waits at most WaitDelay for the output
// pipes. Parsing what was captured is bounded by the stdout size and is left
// to the engine's slack.
func (r *Runner) ShutdownBound() time.Duration { return r.waitDelay() }

func (r *Runner) Run(ctx context.Context, spec ports.RunSpec) (ports.RunResult, error) {
	args, err := buildArgs(spec.Prompt, spec.AgentConfig)
	if err != nil {
		return ports.RunResult{}, err
	}
	// buildArgs already validated spec.AgentConfig; the alias is only
	// needed here as the model-resolution fallback (ADR-0006 §7), so the
	// error is unreachable and safely ignored.
	cfg, _, _ := parseAgentConfig(spec.AgentConfig)

	if err := ctx.Err(); err != nil {
		// Nothing started, so nothing was spent.
		return ports.RunResult{}, fmt.Errorf("claude code run not started: %w", err)
	}

	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = spec.WorkspacePath
	// cmd.Stdin is deliberately left nil: os/exec connects a nil Stdin to
	// the null device, and an explicit *os.File open here could shadow
	// that default. Claude Code headless waits ~3s on a stdin that never
	// closes, so this must never regress.
	stdout := &cappedBuffer{max: r.stdoutLimit()}
	stderr := &tailBuffer{max: stderrTailBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The agent and everything its tools start share one process group:
	// cancellation, and every return from Run, kills all of them.
	sig := r.killSignal()
	configureProcess(cmd, syscall.SIGKILL)
	cmd.Cancel = func() error { return killGroup(cmd, sig) }
	cmd.WaitDelay = r.waitDelay()

	// Pdeathsig kills the CLI when the worker dies, but only the CLI: what
	// it started is reparented and would race this run inside the same
	// workspace. Whatever an earlier run recorded there is killed first.
	killRecordedGroup(spec.WorkspacePath)
	runErr := runProcess(cmd, func(pid int) { recordAgentGroup(spec.WorkspacePath, pid) })
	// Every return from runProcess has already killed the group.
	clearAgentGroupRecord(spec.WorkspacePath)
	if cmd.Process == nil {
		// The CLI never started (missing binary, unsupported platform).
		return ports.RunResult{}, fmt.Errorf("claude code run: %w", runErr)
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// The CLI exited cleanly; something that escaped its group still
		// held the output. What the CLI printed is still read.
		runErr = nil
	}
	return classifyRun(ctx, runErr, stdout, stderr, cfg.Model)
}

// classifyRun turns a finished run into a result or one of the failure
// classes of ADR-0006 §8.
func classifyRun(ctx context.Context, runErr error, stdout *cappedBuffer, stderr *tailBuffer, requestedModel string) (ports.RunResult, error) {
	if stdout.overflowed {
		return ports.RunResult{}, &ports.UnmeteredRunError{
			Err: fmt.Errorf("claude code stdout exceeded %d bytes; the reported cost cannot be trusted%s", stdout.max, stderr.suffix()),
		}
	}
	env, envErr := parseEnvelope(stdout.Bytes())
	if errors.Is(envErr, ports.ErrAmbiguousEnvelope) {
		return ports.RunResult{}, &ports.UnmeteredRunError{Err: fmt.Errorf("claude code run: %w%s", envErr, stderr.suffix())}
	}
	if envErr == nil {
		result := envelopeToResult(env, requestedModel)
		switch {
		case runErr != nil:
			// The CLI reported its cost before failing: the run was billed.
			return ports.RunResult{}, &ports.RunError{
				Result: result,
				Err:    fmt.Errorf("claude code exited with a failure: %w%s", runErr, stderr.suffix()),
			}
		case env.IsError:
			return ports.RunResult{}, &ports.RunError{
				Result: result,
				Err:    fmt.Errorf("claude code reported error: %s", truncateText(env.Result, maxErrorResultBytes)),
			}
		}
		return result, nil
	}

	// No usable envelope: the cost is unknown.
	if runErr == nil || ctx.Err() != nil || killedOrTerminated(runErr) {
		// The CLI ran and ended without reporting — a clean exit, a kill we
		// issued, or a signal it trapped (it exits 128+signo). It may have
		// billed; a retry would bill again.
		cause := runErr
		if cause == nil {
			cause = envErr
		} else if ctx.Err() != nil {
			cause = fmt.Errorf("%w (%w)", runErr, ctx.Err())
		}
		return ports.RunResult{}, &ports.UnmeteredRunError{
			Err: fmt.Errorf("claude code run: %w%s", cause, stderr.suffix()),
		}
	}
	return ports.RunResult{}, fmt.Errorf("claude code run: %w%s", runErr, stderr.suffix())
}

// killedOrTerminated reports a death by signal, or an exit status above 128
// — how a shell or the CLI reports a signal it handled.
func killedOrTerminated(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	code := exitErr.ExitCode()
	return code == -1 || code > 128
}

func (r *Runner) waitDelay() time.Duration {
	if r.WaitDelay > 0 {
		return r.WaitDelay
	}
	return defaultWaitDelay
}

func (r *Runner) stdoutLimit() int {
	if r.stdoutCap > 0 {
		return r.stdoutCap
	}
	return defaultStdoutCap
}

func (r *Runner) killSignal() syscall.Signal {
	if r.cancelSignal != 0 {
		return r.cancelSignal
	}
	return syscall.SIGKILL
}

// cappedBuffer keeps the first max bytes written to it and records whether
// more arrived. It never fails a write, so the process is not disturbed.
type cappedBuffer struct {
	max        int
	buf        bytes.Buffer
	overflowed bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.overflowed = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

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

// truncateText bounds s for an error message, saying explicitly how much
// was dropped. Bytes cut mid-rune are removed, so the message stays valid
// UTF-8 for whatever encodes it downstream.
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	kept := strings.ToValidUTF8(s[:max], "")
	return fmt.Sprintf("%s [truncated to %d of %d bytes]", kept, len(kept), len(s))
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
