// Command worker runs a tollgate Temporal worker: it registers the job
// workflow and its activities, then polls the task queue until interrupted.
package main

import (
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/Benja272/tollgate/internal/adapters/claudecode"
	"github.com/Benja272/tollgate/internal/adapters/postgres"
	"github.com/Benja272/tollgate/internal/engine"
	"github.com/Benja272/tollgate/internal/ports"
)

const defaultDatabaseURL = "postgres://tollgate:tollgate@localhost:5432/tollgate?sslmode=disable"

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

	w := worker.New(c, engine.TaskQueue, worker.Options{})
	w.RegisterWorkflow(engine.JobWorkflow)
	w.RegisterActivity(&engine.Activities{
		Agent: &claudecode.Runner{Bin: "claude"},
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
