package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Benja272/tollgate/internal/ports"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TOLLGATE_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://tollgate:tollgate@localhost:5432/tollgate?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	if err := pool.Ping(context.Background()); err != nil {
		if os.Getenv("TOLLGATE_REQUIRE_POSTGRES") != "" {
			t.Fatalf("TOLLGATE_REQUIRE_POSTGRES is set but postgres is unreachable: %v", err)
		}
		t.Skipf("postgres not reachable, skipping integration test: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func entriesFor(jobID string) []ports.CostEntry {
	return []ports.CostEntry{
		{JobID: jobID, Phase: "run_agent", Actor: "agent", Model: "sonnet", USD: 0.772445, Attempt: 1},
		{JobID: jobID, Phase: "judge", Actor: "judge:sonnet", Model: "sonnet", USD: 0.255358, Attempt: 1},
		{JobID: jobID, Phase: "judge", Actor: "judge:haiku", Model: "haiku", USD: 0.055145, Attempt: 1},
	}
}

func TestLedger_RecordCosts_PersistsAndAggregates(t *testing.T) {
	pool := testPool(t)
	l := NewLedger(pool)
	jobID := fmt.Sprintf("ledger-test-%d", time.Now().UnixNano())

	require.NoError(t, l.RecordCosts(context.Background(), entriesFor(jobID)))

	var total float64
	var rows int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(usd), 0) FROM cost_entries WHERE job_id = $1`, jobID).
		Scan(&rows, &total))
	require.Equal(t, 3, rows)
	require.InDelta(t, 1.082948, total, 1e-9, "the ledger must add up exactly")
}

func TestLedger_RecordCosts_IsIdempotent(t *testing.T) {
	pool := testPool(t)
	l := NewLedger(pool)
	jobID := fmt.Sprintf("ledger-idem-%d", time.Now().UnixNano())

	require.NoError(t, l.RecordCosts(context.Background(), entriesFor(jobID)))
	require.NoError(t, l.RecordCosts(context.Background(), entriesFor(jobID)),
		"an at-least-once retry of the same batch must succeed")

	var rows int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM cost_entries WHERE job_id = $1`, jobID).Scan(&rows))
	require.Equal(t, 3, rows, "a retried write must never double-count money")
}
