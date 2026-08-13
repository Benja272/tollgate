// Command worker runs a tollgate Temporal worker: it registers the job
// workflow and its activities, then polls the task queue until interrupted.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/Benja272/tollgate/internal/adapters/claudecode"
	"github.com/Benja272/tollgate/internal/adapters/postgres"
	"github.com/Benja272/tollgate/internal/engine"
	"github.com/Benja272/tollgate/internal/ports"
	"github.com/Benja272/tollgate/internal/telemetry"
)

const defaultDatabaseURL = "postgres://tollgate:tollgate@localhost:5432/tollgate?sslmode=disable"

// shutdownTimeout bounds the final telemetry flush; an unreachable collector
// must not hold the process open.
const shutdownTimeout = 5 * time.Second

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	dsn := os.Getenv("TOLLGATE_DATABASE_URL")
	if dsn == "" {
		dsn = defaultDatabaseURL
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		log.Fatalf("ledger pool: %v", err)
	}
	defer pool.Close()

	// Telemetry is best-effort by construction: a failed setup logs and the
	// worker runs blind rather than not running at all.
	instruments, shutdownTelemetry, err := telemetry.Setup(context.Background(), telemetry.ConfigFromEnv())
	if err != nil {
		log.Printf("telemetry disabled (setup failed): %v", err)
	} else {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := shutdownTelemetry(ctx); err != nil {
				log.Printf("telemetry shutdown: %v", err)
			}
		}()
	}

	w := worker.New(c, engine.TaskQueue, worker.Options{})
	w.RegisterWorkflow(engine.JobWorkflow)
	w.RegisterActivity(&engine.Activities{
		Agent:     &claudecode.Runner{Bin: "claude"},
		AgentName: "claude-code",
		Telemetry: instruments,
		Judges: map[string]ports.Judge{
			"haiku":  &claudecode.CLIJudge{Bin: "claude", Model: "haiku"},
			"sonnet": &claudecode.CLIJudge{Bin: "claude", Model: "sonnet"},
			"opus":   &claudecode.CLIJudge{Bin: "claude", Model: "opus"},
		},
		Ledger: postgres.NewLedger(pool),
	})

	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker: %v", err)
	}
}
