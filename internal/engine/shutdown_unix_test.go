//go:build unix

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Benja272/tollgate/internal/adapters/claudecode"
)

// The margin must cover the adapter's whole shutdown path — group kill,
// WaitDelay for a process that escaped the group and still holds stdout,
// envelope parse, returning the error — or the server times the attempt out
// first and retries it. This pins that with the real runner.
func TestActivities_RunAgent_RealRunnerShutdownFinishesBeforeActivityDeadline(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("setsid not available: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escaped.pid")
	bin := filepath.Join(dir, "claude")
	// The escaped child leaves the process group, so the group kill cannot
	// close its copy of stdout: only WaitDelay ends the wait.
	script := "#!/bin/sh\nsetsid sh -c 'echo $$ > " + pidFile + "; exec sleep 60' &\nexec sleep 60\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	acts := &Activities{
		Agent:             &claudecode.Runner{Bin: bin, WaitDelay: 3 * time.Second},
		HeartbeatInterval: time.Hour,
	}
	// One second of agent time on top of the derived margin: a margin that
	// ignored the WaitDelay would still be shutting down at the deadline.
	timeout := 3*time.Second + runReturnSlack + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	_, err := acts.RunAgent(ctx, RunAgentInput{JobID: "job-s", Workspace: Workspace{Path: dir}, Prompt: "render", Attempt: 1})
	elapsed := time.Since(start)

	requireNonRetryableType(t, err, errTypeAgentRunUnmetered)
	require.NoError(t, ctx.Err(), "shutdown took %s of a %s activity budget", elapsed, timeout)
	require.GreaterOrEqual(t, elapsed, time.Second+3*time.Second, "the WaitDelay path was exercised")
}

// --- B9: billed failures reach telemetry, details stay small ---------------
