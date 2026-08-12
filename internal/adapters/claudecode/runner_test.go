package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	bin := fakeClaude(t, `echo '{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.0123,"result":"implemented the ticket","session_id":"sess-1"}'`)
	r := &Runner{Bin: bin}

	got, err := r.Run(context.Background(), ports.RunSpec{
		WorkspacePath: t.TempDir(),
		Prompt:        "implement the ticket",
	})

	require.NoError(t, err)
	require.InDelta(t, 0.0123, got.CostUSD, 1e-9)
	require.Equal(t, "implemented the ticket", got.Output)
	require.Equal(t, "sess-1", got.SessionID)
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
