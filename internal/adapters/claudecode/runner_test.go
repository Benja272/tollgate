package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
