package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/Benja272/tollgate/internal/adapters/claudecode"
	"github.com/Benja272/tollgate/internal/adapters/gitcli"
	"github.com/Benja272/tollgate/internal/adapters/postgres"
	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/workspace"
)

// e2eFakeLedger is the "else an in-memory ports.LedgerStore fake" the task
// describes: it records exactly the fields a real postgres.Ledger would
// persist, without requiring a reachable database for this E2E test.
type e2eFakeLedger struct {
	mu      sync.Mutex
	entries []ports.CostEntry
}

func (l *e2eFakeLedger) RecordCosts(ctx context.Context, entries []ports.CostEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entries...)
	return nil
}

func (l *e2eFakeLedger) recorded() []ports.CostEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ports.CostEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// e2eGitRepo creates a one-commit real git repository and returns its
// absolute path and commit SHA, for a real gitcli.Checkout to check out.
func e2eGitRepo(t *testing.T) (repo, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	repo = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=tollgate-e2e", "GIT_AUTHOR_EMAIL=e2e@tollgate.local",
			"GIT_COMMITTER_NAME=tollgate-e2e", "GIT_COMMITTER_EMAIL=e2e@tollgate.local",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture repo\n"), 0o644))
	run("add", ".")
	run("commit", "-q", "-m", "fixture commit")
	return repo, run("rev-parse", "HEAD")
}

// e2eFakeClaude writes a fake `claude` binary that ignores its arguments and
// emits the given result envelope, so the E2E test never invokes (or pays
// for) the real agent.
func e2eFakeClaude(t *testing.T, envelope string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ncat <<'JSON'\n" + envelope + "\nJSON\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// e2eWorker starts a worker that registers ArtifactJobWorkflow and ONLY the
// five activity methods it actually calls — never the whole *Activities
// struct — so an accidental Ship/JudgeOne call fails loudly (no such
// activity registered) instead of silently running the real scaffolding.
func e2eWorker(t *testing.T, c client.Client, taskQueue string, acts *Activities) worker.Worker {
	t.Helper()
	w := worker.New(c, taskQueue, worker.Options{WorkerStopTimeout: time.Second})
	w.RegisterWorkflow(ArtifactJobWorkflow)
	w.RegisterActivity(acts.ValidateAgentConfig)
	w.RegisterActivity(acts.CheckoutWorkspace)
	w.RegisterActivity(acts.ApplyOverlay)
	w.RegisterActivity(acts.RunAgent)
	w.RegisterActivity(acts.RecordCosts)
	require.NoError(t, w.Start())
	return w
}

func dialOrSkip(t *testing.T) client.Client {
	t.Helper()
	c, err := client.Dial(client.Options{})
	if err != nil {
		if os.Getenv("TOLLGATE_REQUIRE_TEMPORAL") != "" {
			t.Fatalf("TOLLGATE_REQUIRE_TEMPORAL is set but the dev server is unreachable: %v", err)
		}
		t.Skipf("temporal dev server not reachable, skipping integration test: %v", err)
	}
	return c
}

// TestArtifactJobWorkflow_E2E_RealServer_RecordsCostNoPR is the
// [Spec: artifact-job#No PR, No Gate] end-to-end scenario: a real Temporal
// dev server, a real git checkout, a real overlay onto a scratch root, and
// claudecode.Runner pointed at a fake binary. It asserts the job completes,
// its run_agent cost row carries the model and piece_id, and neither Ship
// nor JudgeOne is ever scheduled.
func TestArtifactJobWorkflow_E2E_RealServer_RecordsCostNoPR(t *testing.T) {
	c := dialOrSkip(t)
	defer c.Close()

	repo, sha := e2eGitRepo(t)
	workspaceRoot := t.TempDir()
	overlaySrcDir := t.TempDir()
	overlaySrc := filepath.Join(overlaySrcDir, "plan.md")
	require.NoError(t, os.WriteFile(overlaySrc, []byte("the plan"), 0o644))

	claudeBin := e2eFakeClaude(t, `{"type":"result","is_error":false,"total_cost_usd":0.42,"result":"planned","session_id":"sess-e2e","modelUsage":{"claude-3-5-sonnet":{"costUSD":0.42}}}`)

	ledger := &e2eFakeLedger{}
	acts := &Activities{
		Agent:             &claudecode.Runner{Bin: claudeBin},
		Checkout:          gitcli.Checkout{},
		Ledger:            ledger,
		WorkspaceRoot:     workspaceRoot,
		HeartbeatInterval: time.Hour,
	}

	taskQueue := fmt.Sprintf("tollgate-artifact-e2e-%d", time.Now().UnixNano())
	w := e2eWorker(t, c, taskQueue, acts)
	defer w.Stop()

	jobID := fmt.Sprintf("e2e-success-%d", time.Now().UnixNano())
	in := ArtifactJobInput{
		JobID:            jobID,
		PieceID:          "piece-e2e-1",
		Repo:             repo,
		SourceRef:        sha,
		Prompt:           "draft the plan",
		AgentConfig:      json.RawMessage(`{"model":"sonnet"}`),
		DestinationRoots: []string{"output"},
		Overlays:         []workspace.Overlay{{Source: overlaySrc, Dest: "output/plan.md"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "artifact-" + jobID, TaskQueue: taskQueue,
	}, ArtifactJobWorkflow, in)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = c.TerminateWorkflow(context.Background(), "artifact-"+jobID, "", "test cleanup")
	})

	var result ArtifactJobResult
	require.NoError(t, run.Get(ctx, &result), "artifact job did not complete")
	require.Equal(t, "claude-3-5-sonnet", result.Model)
	require.InDelta(t, 0.42, result.CostUSD, 1e-9)

	got, readErr := os.ReadFile(filepath.Join(workspaceRoot, "tollgate-artifact-"+jobID, "output", "plan.md"))
	require.NoError(t, readErr)
	require.Equal(t, "the plan", string(got))

	entries := ledger.recorded()
	require.Len(t, entries, 1)
	require.Equal(t, "run_agent", entries[0].Phase)
	require.Equal(t, "claude-3-5-sonnet", entries[0].Model)
	require.Equal(t, "piece-e2e-1", entries[0].PieceID)
}

// TestArtifactJobWorkflow_E2E_BilledFailure_RowStillLands asserts the
// [Spec: cost-ledger#Billed Failure Is Recorded Before the Job Fails]
// artifact-job scenario end-to-end: a billed failure still lands its
// run_agent row before the workflow fails.
func TestArtifactJobWorkflow_E2E_BilledFailure_RowStillLands(t *testing.T) {
	c := dialOrSkip(t)
	defer c.Close()

	repo, sha := e2eGitRepo(t)
	workspaceRoot := t.TempDir()
	overlaySrcDir := t.TempDir()
	overlaySrc := filepath.Join(overlaySrcDir, "plan.md")
	require.NoError(t, os.WriteFile(overlaySrc, []byte("the plan"), 0o644))

	claudeBin := e2eFakeClaude(t, `{"type":"result","is_error":true,"total_cost_usd":0.07,"result":"denied","modelUsage":{"claude-3-5-haiku":{"costUSD":0.07}}}`)

	ledger := &e2eFakeLedger{}
	acts := &Activities{
		Agent:             &claudecode.Runner{Bin: claudeBin},
		Checkout:          gitcli.Checkout{},
		Ledger:            ledger,
		WorkspaceRoot:     workspaceRoot,
		HeartbeatInterval: time.Hour,
	}

	taskQueue := fmt.Sprintf("tollgate-artifact-e2e-billed-%d", time.Now().UnixNano())
	w := e2eWorker(t, c, taskQueue, acts)
	defer w.Stop()

	jobID := fmt.Sprintf("e2e-billed-%d", time.Now().UnixNano())
	in := ArtifactJobInput{
		JobID:            jobID,
		PieceID:          "piece-e2e-2",
		Repo:             repo,
		SourceRef:        sha,
		Prompt:           "draft the plan",
		AgentConfig:      json.RawMessage(`{"model":"haiku"}`),
		DestinationRoots: []string{"output"},
		Overlays:         []workspace.Overlay{{Source: overlaySrc, Dest: "output/plan.md"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "artifact-" + jobID, TaskQueue: taskQueue,
	}, ArtifactJobWorkflow, in)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = c.TerminateWorkflow(context.Background(), "artifact-"+jobID, "", "test cleanup")
	})

	err = run.Get(ctx, nil)
	require.Error(t, err, "a billed failure must still fail the job")

	entries := ledger.recorded()
	require.Len(t, entries, 1, "the billed run_agent row must land before the job fails")
	require.Equal(t, "claude-3-5-haiku", entries[0].Model)
	require.Equal(t, "piece-e2e-2", entries[0].PieceID)
	require.InDelta(t, 0.07, entries[0].USD, 1e-9)
}

// ledgerPoolOrSkip connects to the test database the postgres adapter tests
// use, honoring TOLLGATE_REQUIRE_POSTGRES.
func ledgerPoolOrSkip(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TOLLGATE_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://tollgate:tollgate@localhost:5432/tollgate?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		if os.Getenv("TOLLGATE_REQUIRE_POSTGRES") != "" {
			t.Fatalf("TOLLGATE_REQUIRE_POSTGRES is set but postgres is unreachable: %v", err)
		}
		t.Skipf("postgres not reachable, skipping integration test: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestArtifactJobWorkflow_E2E_SameJobIDTwice_BothExecutionsLedgered is the
// review-B8 scenario end to end: a real Temporal server and a real Postgres
// ledger, the same JobID executed twice (a model comparison re-run), each
// execution billing a different amount. Both rows must land and the
// per-piece total must be their sum.
func TestArtifactJobWorkflow_E2E_SameJobIDTwice_BothExecutionsLedgered(t *testing.T) {
	c := dialOrSkip(t)
	defer c.Close()
	ledger := postgres.NewLedger(ledgerPoolOrSkip(t))

	repo, sha := e2eGitRepo(t)
	workspaceRoot := t.TempDir()
	overlaySrc := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(overlaySrc, []byte("the plan"), 0o644))

	// The fake agent bills 0.50 on its first run and 0.80 on its second.
	state := filepath.Join(t.TempDir(), "runs")
	claudeBin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
if [ -f "` + state + `" ]; then cost=0.80; else cost=0.50; touch "` + state + `"; fi
printf '{"type":"result","is_error":false,"total_cost_usd":%s,"result":"ok","modelUsage":{"claude-haiku-4-5":{"costUSD":%s}}}\n' "$cost" "$cost"
`
	require.NoError(t, os.WriteFile(claudeBin, []byte(script), 0o755))

	acts := &Activities{
		Agent:             &claudecode.Runner{Bin: claudeBin},
		Checkout:          gitcli.Checkout{},
		Ledger:            ledger,
		WorkspaceRoot:     workspaceRoot,
		HeartbeatInterval: time.Hour,
	}
	taskQueue := fmt.Sprintf("tollgate-artifact-e2e-rerun-%d", time.Now().UnixNano())
	w := e2eWorker(t, c, taskQueue, acts)
	defer w.Stop()

	stamp := time.Now().UnixNano()
	jobID := fmt.Sprintf("e2e-rerun-%d", stamp)
	piece := fmt.Sprintf("piece-e2e-rerun-%d", stamp)
	in := ArtifactJobInput{
		JobID:            jobID,
		PieceID:          piece,
		Repo:             repo,
		SourceRef:        sha,
		Prompt:           "render the piece",
		AgentConfig:      json.RawMessage(`{"model":"haiku"}`),
		DestinationRoots: []string{"output"},
		Overlays:         []workspace.Overlay{{Source: overlaySrc, Dest: "output/plan.md"}},
	}
	workflowID := "artifact-" + jobID
	t.Cleanup(func() {
		_ = c.TerminateWorkflow(context.Background(), workflowID, "", "test cleanup")
	})

	runIDs := map[string]bool{}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: taskQueue}, ArtifactJobWorkflow, in)
		require.NoError(t, err)
		require.NoError(t, run.Get(ctx, nil), "execution %d did not complete", i+1)
		cancel()
		runIDs[run.GetRunID()] = true

		// The overlay left untracked files, so the checkout is not reusable;
		// an operator removes the worktree before re-running (ADR-0006).
		out, err := exec.Command("git", "-C", repo, "worktree", "remove", "--force",
			filepath.Join(workspaceRoot, "tollgate-artifact-"+jobID)).CombinedOutput()
		require.NoError(t, err, "git worktree remove: %s", out)
	}
	require.Len(t, runIDs, 2, "two distinct executions")

	spend, err := ledger.PerPieceSpend(context.Background(), piece)
	require.NoError(t, err)
	require.Len(t, spend, 1)
	require.Equal(t, "claude-haiku-4-5", spend[0].Model)
	require.InDelta(t, 1.30, spend[0].USD, 1e-9, "both executions' spend must be in the ledger")
}
