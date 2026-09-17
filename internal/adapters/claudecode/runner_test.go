package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Benja272/tollgate/internal/ports"
)

// fakeClaude writes an executable script that mimics `claude -p
// --output-format json` so adapter tests never invoke (or pay for) the real
// agent.
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	return path
}

func TestRunner_Run_ParsesResultJSON(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.0123,"result":"implemented the ticket","session_id":"sess-1","usage":{"input_tokens":10,"output_tokens":84,"cache_read_input_tokens":18282,"cache_creation_input_tokens":23716}}'`)
	r := &Runner{Bin: bin}

	got, err := r.Run(context.Background(), ports.RunSpec{
		WorkspacePath: t.TempDir(),
		Prompt:        "implement the ticket",
	})

	require.NoError(t, err)
	require.InDelta(t, 0.0123, got.CostUSD, 1e-9)
	require.Equal(t, "implemented the ticket", got.Output)
	require.Equal(t, "sess-1", got.SessionID)
	require.Equal(t, ports.TokenUsage{
		InputTokens:         10,
		OutputTokens:        84,
		CacheReadTokens:     18282,
		CacheCreationTokens: 23716,
	}, got.Usage, "all four token classes must survive the adapter — cache tokens dominate cost")
}

func TestRunner_Run_RunsInsideWorkspace(t *testing.T) {
	bin := fakeClaude(t, `printf '{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.01,"result":"%s","session_id":"sess-2"}' "$PWD"`)
	r := &Runner{Bin: bin}
	workspace := t.TempDir()

	got, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: workspace, Prompt: "noop"})

	require.NoError(t, err)
	require.Equal(t, workspace, got.Output, "agent must execute with the workspace as working directory")
}

func TestRunner_Run_NonZeroExit_ReturnsError(t *testing.T) {
	bin := fakeClaude(t, `echo "boom" >&2; exit 1`)
	r := &Runner{Bin: bin}

	_, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "noop"})

	require.Error(t, err)
}

func TestRunner_Run_PassesAgentConfigIntoArgs(t *testing.T) {
	bin := fakeClaude(t, `printf '%s\n' "$@" > "$PWD/args.txt"; echo '{"type":"result","is_error":false,"total_cost_usd":0.01,"result":"ok"}'`)
	r := &Runner{Bin: bin}
	workspace := t.TempDir()
	cfg := json.RawMessage(`{"model":"sonnet","tools":["Read"],"allowed_tools":["Read"]}`)

	_, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: workspace, Prompt: "do it", AgentConfig: cfg})
	require.NoError(t, err)

	wantArgs, buildErr := buildArgs("do it", cfg)
	require.NoError(t, buildErr)

	got, readErr := os.ReadFile(filepath.Join(workspace, "args.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, strings.Join(wantArgs, "\n")+"\n", string(got))
}

func TestRunner_Run_StdinIsDevNull(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd/0"); err != nil {
		t.Skip("requires /proc (Linux)")
	}
	bin := fakeClaude(t, `readlink /proc/self/fd/0 > "$PWD/stdin.txt"; echo '{"type":"result","is_error":false,"total_cost_usd":0.01,"result":"ok"}'`)
	r := &Runner{Bin: bin}
	workspace := t.TempDir()

	_, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: workspace, Prompt: "noop"})
	require.NoError(t, err)

	got, readErr := os.ReadFile(filepath.Join(workspace, "stdin.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "/dev/null\n", string(got))
}

func TestRunner_Run_ModelSelection_HighestCostWins(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","is_error":false,"total_cost_usd":0.06,"result":"ok","modelUsage":{"claude-3-5-haiku":{"costUSD":0.01},"claude-3-5-sonnet":{"costUSD":0.05}}}'`)
	r := &Runner{Bin: bin}

	got, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "noop"})

	require.NoError(t, err)
	assert.Equal(t, "claude-3-5-sonnet", got.Model)
}

func TestRunner_Run_ModelSelection_TiesBreakOnSmallestID(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","is_error":false,"total_cost_usd":0.1,"result":"ok","modelUsage":{"claude-3-5-sonnet":{"costUSD":0.05},"claude-3-5-haiku":{"costUSD":0.05}}}'`)
	r := &Runner{Bin: bin}

	got, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "noop"})

	require.NoError(t, err)
	assert.Equal(t, "claude-3-5-haiku", got.Model, "ties break on the lexicographically smallest model id")
}

func TestRunner_Run_ModelSelection_FallsBackToRequestedAlias(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","is_error":false,"total_cost_usd":0.02,"result":"ok"}'`)
	r := &Runner{Bin: bin}

	got, err := r.Run(context.Background(), ports.RunSpec{
		WorkspacePath: t.TempDir(), Prompt: "noop",
		AgentConfig: json.RawMessage(`{"model":"sonnet"}`),
	})

	require.NoError(t, err)
	assert.Equal(t, "sonnet", got.Model)
}

func TestRunner_Run_ModelSelection_UnknownWhenNoUsageAndNoAlias(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","is_error":false,"total_cost_usd":0.02,"result":"ok"}'`)
	r := &Runner{Bin: bin}

	got, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "noop"})

	require.NoError(t, err)
	assert.Equal(t, ports.ModelUnknown, got.Model)
}

func TestRunner_Run_PermissionDenialWithExitZero_IsSuccess(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","is_error":false,"total_cost_usd":0.01,"result":"ok","permission_denials":[{"tool_name":"Bash","tool_input":{}}]}'`)
	r := &Runner{Bin: bin}

	got, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "noop"})

	require.NoError(t, err)
	assert.Equal(t, "ok", got.Output)
}

func TestRunner_Run_IsErrorTrue_ReturnsRunErrorWithPartialResult(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","is_error":true,"total_cost_usd":0.02,"result":"denied: no fs access","modelUsage":{"claude-3-5-haiku":{"costUSD":0.02}}}'`)
	r := &Runner{Bin: bin}

	_, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "noop"})

	require.Error(t, err)
	var runErr *ports.RunError
	require.ErrorAs(t, err, &runErr)
	assert.InDelta(t, 0.02, runErr.Result.CostUSD, 1e-9)
	assert.Equal(t, "claude-3-5-haiku", runErr.Result.Model)
}

func TestRunner_Run_NonZeroExitWithParseableEnvelope_ReturnsRunError(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","is_error":false,"total_cost_usd":0.03,"result":"partial"}'; exit 1`)
	r := &Runner{Bin: bin}

	_, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "noop"})

	require.Error(t, err)
	var runErr *ports.RunError
	require.ErrorAs(t, err, &runErr)
	assert.InDelta(t, 0.03, runErr.Result.CostUSD, 1e-9)
}

func TestRunner_Run_NonZeroExitWithGarbageOutput_ReturnsPlainError(t *testing.T) {
	bin := fakeClaude(t, `echo "garbage, not json"; exit 1`)
	r := &Runner{Bin: bin}

	_, err := r.Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "noop"})

	require.Error(t, err)
	var runErr *ports.RunError
	assert.False(t, errors.As(err, &runErr), "unparseable output must stay a plain, retryable error")
}

const okEnvelope = `{"type":"result","is_error":false,"total_cost_usd":0.07,"result":"ok","modelUsage":{"claude-haiku-4-5":{"costUSD":0.07}}}`

func TestRunner_Run_PromptStartingWithDash_ReachesCLIAsOperand(t *testing.T) {
	bin := fakeClaude(t, `printf '%s\n' "$@" > "$PWD/args.txt"; echo '`+okEnvelope+`'`)
	workspace := t.TempDir()

	_, err := (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: workspace, Prompt: "--version"})
	require.NoError(t, err)

	got, readErr := os.ReadFile(filepath.Join(workspace, "args.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "-p\n--output-format\njson\n--\n--version\n", string(got))
}

func TestRunner_Run_ContextDeadline_ReturnsUnmeteredRunError(t *testing.T) {
	bin := fakeClaude(t, `sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := (&Runner{Bin: bin}).Run(ctx, ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "render"})

	require.Less(t, time.Since(start), 10*time.Second, "a cancelled run must not wait for the agent")
	var unmetered *ports.UnmeteredRunError
	require.ErrorAs(t, err, &unmetered, "a killed run may have billed: it must never read as nothing-billed")
	var billed *ports.RunError
	assert.False(t, errors.As(err, &billed))
}

func TestRunner_Run_KilledBySignal_ReturnsUnmeteredRunError(t *testing.T) {
	bin := fakeClaude(t, `kill -9 $$`)

	_, err := (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})

	var unmetered *ports.UnmeteredRunError
	require.ErrorAs(t, err, &unmetered)
}

func TestRunner_Run_KilledAfterPrintingEnvelope_IsBilled(t *testing.T) {
	bin := fakeClaude(t, `echo '`+okEnvelope+`'; kill -9 $$`)

	_, err := (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})

	var billed *ports.RunError
	require.ErrorAs(t, err, &billed, "an envelope was printed: the spend is known and must be recorded")
	assert.InDelta(t, 0.07, billed.Result.CostUSD, 1e-9)
}

// A background process started by the agent's Bash tool inherits stdout;
// without WaitDelay the runner blocked until the activity timed out and the
// billed envelope was lost (review B7).
func TestRunner_Run_BackgroundChildHoldsStdout_EnvelopeStillParsedChildKilled(t *testing.T) {
	workspace := t.TempDir()
	bin := fakeClaude(t, `sleep 60 & echo $! > "$PWD/child.pid"; echo '`+okEnvelope+`'`)

	start := time.Now()
	got, err := (&Runner{Bin: bin, WaitDelay: 200 * time.Millisecond}).Run(context.Background(),
		ports.RunSpec{WorkspacePath: workspace, Prompt: "p"})

	require.NoError(t, err)
	require.Less(t, time.Since(start), 10*time.Second)
	assert.InDelta(t, 0.07, got.CostUSD, 1e-9)

	raw, readErr := os.ReadFile(filepath.Join(workspace, "child.pid"))
	require.NoError(t, readErr)
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, convErr)
	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 5*time.Second, 50*time.Millisecond, "the agent's leftover process group must be killed")
}

func TestRunner_Run_Failure_CarriesBoundedStderrTail(t *testing.T) {
	bin := fakeClaude(t, `head -c 100000 /dev/zero | tr '\0' 'x' >&2; echo "auth failed: token expired" >&2; exit 1`)

	_, err := (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth failed: token expired")
	assert.Less(t, len(err.Error()), 8192, "stderr must be bounded in the error")
}

func TestRunner_Run_EnvelopeAmongNoiseLines_IsParsed(t *testing.T) {
	bin := fakeClaude(t, `echo "warning: something"; echo '{"note":"not the envelope"}'; echo '`+okEnvelope+`'; echo "bye"`)

	got, err := (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})

	require.NoError(t, err)
	assert.InDelta(t, 0.07, got.CostUSD, 1e-9)
}

// Without a usable envelope the cost is unknown. A clean exit means the CLI
// ran — and may have billed — so it is unmetered, never retried (review
// R4). A non-zero exit of its own (1..128) stays a plain retryable error.
func TestRunner_Run_NoUsableEnvelope_ExitZeroIsUnmeteredExitOneIsPlain(t *testing.T) {
	for name, out := range map[string]string{
		"no output":       ``,
		"null":            `null`,
		"empty object":    `{}`,
		"wrong type":      `{"type":"assistant","total_cost_usd":0.1,"result":"x"}`,
		"cost missing":    `{"type":"result","is_error":false,"result":"x"}`,
		"cost not number": `{"type":"result","is_error":false,"total_cost_usd":"0.1","result":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			bin := fakeClaude(t, `printf '%s\n' '`+out+`'; exit 0`)
			_, err := (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})
			var unmetered *ports.UnmeteredRunError
			require.ErrorAs(t, err, &unmetered, "exit 0 without a usable envelope")

			bin = fakeClaude(t, `printf '%s\n' '`+out+`'; exit 1`)
			_, err = (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})
			require.Error(t, err)
			var billed *ports.RunError
			assert.False(t, errors.As(err, &billed), "%q is not an envelope", out)
			assert.False(t, errors.As(err, &unmetered), "exit 1 of its own stays retryable")
		})
	}
}

// Claude Code traps SIGTERM/SIGHUP and exits 128+signo without an
// envelope; a systemd stop does exactly that (review R3, reproduced).
func TestRunner_Run_ExitAbove128WithoutEnvelope_IsUnmetered(t *testing.T) {
	for _, code := range []string{"129", "143", "255"} {
		t.Run(code, func(t *testing.T) {
			bin := fakeClaude(t, `exit `+code)
			_, err := (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})
			var unmetered *ports.UnmeteredRunError
			require.ErrorAs(t, err, &unmetered)
		})
	}
	t.Run("128 stays plain", func(t *testing.T) {
		bin := fakeClaude(t, `exit 128`)
		_, err := (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})
		require.Error(t, err)
		var unmetered *ports.UnmeteredRunError
		assert.False(t, errors.As(err, &unmetered))
	})
}

// Anything that inherited stdout can print a line; with more than one
// result envelope the real one cannot be told apart (review R5,
// reproduced with a forged cost-0 line after the real one).
func TestRunner_Run_EnvelopeCount(t *testing.T) {
	const forged = `{"type":"result","is_error":false,"total_cost_usd":0,"result":"forged"}`
	cases := map[string]struct {
		script    string
		ambiguous bool
	}{
		"exactly one":   {script: `echo '` + okEnvelope + `'`},
		"forged after":  {script: `echo '` + okEnvelope + `'; echo '` + forged + `'`, ambiguous: true},
		"forged before": {script: `echo '` + forged + `'; echo '` + okEnvelope + `'`, ambiguous: true},
		// The leader lingers briefly so the child leaves the group before
		// the group kill that follows the leader's exit.
		"forged by an escaped child after exit": {
			script:    `setsid sh -c 'sleep 0.3; echo '"'"'` + forged + `'"'"'' & sleep 0.1; echo '` + okEnvelope + `'`,
			ambiguous: true,
		},
		"result line without cost next to a real one": {
			script:    `echo '{"type":"result","result":"x"}'; echo '` + okEnvelope + `'`,
			ambiguous: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bin := fakeClaude(t, tc.script)
			got, err := (&Runner{Bin: bin, WaitDelay: 2 * time.Second}).Run(context.Background(),
				ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})
			if !tc.ambiguous {
				require.NoError(t, err)
				assert.InDelta(t, 0.07, got.CostUSD, 1e-9)
				return
			}
			var unmetered *ports.UnmeteredRunError
			require.ErrorAs(t, err, &unmetered)
			require.ErrorIs(t, err, ports.ErrAmbiguousEnvelope)
			assert.Zero(t, got.CostUSD)
		})
	}
}

// Processes the agent left behind (a watcher, a dev server) keep mutating
// the workspace; the group is killed on every return (review R6).
func TestRunner_Run_LeftoverProcessesKilledOnEveryReturn(t *testing.T) {
	cases := map[string]string{
		"non-zero exit after an envelope": `echo '` + okEnvelope + `'; exit 1`,
		"clean exit":                      `echo '` + okEnvelope + `'`,
		"exit without envelope":           `exit 3`,
	}
	for name, tail := range cases {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			bin := fakeClaude(t, `sleep 60 >/dev/null 2>&1 & echo $! > "$PWD/child.pid"; `+tail)

			_, _ = (&Runner{Bin: bin}).Run(context.Background(), ports.RunSpec{WorkspacePath: workspace, Prompt: "p"})

			raw, err := os.ReadFile(filepath.Join(workspace, "child.pid"))
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			require.NoError(t, err)
			require.Eventually(t, func() bool { return !processAlive(pid) }, 2*time.Second, 20*time.Millisecond,
				"the leftover process must be dead once Run returns")
		})
	}
}

// processAlive reports whether pid is a live, non-zombie process.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) == 0 || fields[0] != "Z"
}

func TestRunner_Run_StdoutOverCap_IsUnmetered(t *testing.T) {
	bin := fakeClaude(t, `head -c 4096 /dev/zero | tr '\0' 'x'; echo; echo '`+okEnvelope+`'`)

	_, err := (&Runner{Bin: bin, stdoutCap: 1024}).Run(context.Background(), ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})

	var unmetered *ports.UnmeteredRunError
	require.ErrorAs(t, err, &unmetered)
	assert.Contains(t, err.Error(), "stdout")
}

// A process that handles the cancellation signal and exits with a small
// code of its own is still a run we killed: the context decides.
func TestRunner_Run_CancelledRunExitingOnItsOwn_IsUnmetered(t *testing.T) {
	bin := fakeClaude(t, `trap 'exit 1' TERM; sleep 30 & wait`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := (&Runner{Bin: bin, cancelSignal: syscall.SIGTERM}).Run(ctx, ports.RunSpec{WorkspacePath: t.TempDir(), Prompt: "p"})

	var unmetered *ports.UnmeteredRunError
	require.ErrorAs(t, err, &unmetered)
}

func TestRunner_Run_ContextDoneBeforeStart_NotStartedNotUnmetered(t *testing.T) {
	workspace := t.TempDir()
	bin := fakeClaude(t, `touch "$PWD/ran"; echo '`+okEnvelope+`'`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&Runner{Bin: bin}).Run(ctx, ports.RunSpec{WorkspacePath: workspace, Prompt: "p"})

	require.ErrorIs(t, err, context.Canceled)
	var unmetered *ports.UnmeteredRunError
	assert.False(t, errors.As(err, &unmetered), "nothing ran, nothing was spent")
	_, statErr := os.Stat(filepath.Join(workspace, "ran"))
	assert.True(t, os.IsNotExist(statErr))
}
