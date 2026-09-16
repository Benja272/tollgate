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
		{JobID: jobID, Phase: "run_agent", Actor: "agent", Model: "sonnet", USD: 0.772445, Attempt: 1,
			Usage: ports.TokenUsage{InputTokens: 10, OutputTokens: 84, CacheReadTokens: 18282, CacheCreationTokens: 23716}},
		{JobID: jobID, Phase: "judge", Actor: "judge:sonnet", Model: "sonnet", USD: 0.255358, Attempt: 1,
			Usage: ports.TokenUsage{InputTokens: 500, OutputTokens: 60}},
		{JobID: jobID, Phase: "judge", Actor: "judge:haiku", Model: "haiku", USD: 0.055145, Attempt: 1,
			Usage: ports.TokenUsage{InputTokens: 500, OutputTokens: 55}},
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

	var inTok, outTok, cacheRead, cacheCreate int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens
		 FROM cost_entries WHERE job_id = $1 AND actor = 'agent'`, jobID).
		Scan(&inTok, &outTok, &cacheRead, &cacheCreate))
	require.Equal(t, int64(10), inTok)
	require.Equal(t, int64(84), outTok)
	require.Equal(t, int64(18282), cacheRead)
	require.Equal(t, int64(23716), cacheCreate)
}

func TestLedger_RecordCosts_PieceIDGiven_RecordedOnEveryRow(t *testing.T) {
	pool := testPool(t)
	l := NewLedger(pool)
	jobID := fmt.Sprintf("ledger-piece-%d", time.Now().UnixNano())
	entries := entriesFor(jobID)
	for i := range entries {
		entries[i].PieceID = "piece-77"
	}
	require.NoError(t, l.RecordCosts(context.Background(), entries))

	rows, err := pool.Query(context.Background(), `SELECT piece_id FROM cost_entries WHERE job_id = $1`, jobID)
	require.NoError(t, err)
	defer rows.Close()
	count := 0
	for rows.Next() {
		var pieceID string
		require.NoError(t, rows.Scan(&pieceID))
		require.Equal(t, "piece-77", pieceID)
		count++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 3, count)
}

func TestLedger_RecordCosts_PieceIDOmitted_NullOnEveryRow(t *testing.T) {
	pool := testPool(t)
	l := NewLedger(pool)
	jobID := fmt.Sprintf("ledger-nopiece-%d", time.Now().UnixNano())
	require.NoError(t, l.RecordCosts(context.Background(), entriesFor(jobID)))

	rows, err := pool.Query(context.Background(), `SELECT piece_id FROM cost_entries WHERE job_id = $1`, jobID)
	require.NoError(t, err)
	defer rows.Close()
	count := 0
	for rows.Next() {
		var pieceID *string
		require.NoError(t, rows.Scan(&pieceID))
		require.Nil(t, pieceID, "piece_id must be NULL, not an empty string, when omitted")
		count++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 3, count)
}

func TestLedger_PerPieceSpend_SumsAcrossJobsIncludingFailed(t *testing.T) {
	pool := testPool(t)
	l := NewLedger(pool)
	piece := fmt.Sprintf("piece-spend-%d", time.Now().UnixNano())

	// job3 simulates a job that failed after the agent ran: only its
	// run_agent row exists, no judge rows — spend must still include it.
	require.NoError(t, l.RecordCosts(context.Background(), []ports.CostEntry{
		{JobID: piece + "-job1", Phase: "run_agent", Actor: "agent", Model: "sonnet", USD: 1.0, Attempt: 1, PieceID: piece},
	}))
	require.NoError(t, l.RecordCosts(context.Background(), []ports.CostEntry{
		{JobID: piece + "-job2", Phase: "run_agent", Actor: "agent", Model: "sonnet", USD: 2.0, Attempt: 1, PieceID: piece},
	}))
	require.NoError(t, l.RecordCosts(context.Background(), []ports.CostEntry{
		{JobID: piece + "-job3", Phase: "run_agent", Actor: "agent", Model: "sonnet", USD: 0.5, Attempt: 1, PieceID: piece},
	}))

	spend, err := l.PerPieceSpend(context.Background(), piece)
	require.NoError(t, err)
	require.Len(t, spend, 1)
	require.Equal(t, "sonnet", spend[0].Model)
	require.InDelta(t, 3.5, spend[0].USD, 1e-9, "per-piece spend must include the failed job's recorded row")
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
