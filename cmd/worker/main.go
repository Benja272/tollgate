// Command worker runs a tollgate Temporal worker: it registers the job
// workflow and its activities, then polls the task queue until interrupted.
package main

import (
	"log"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/Benja272/tollgate/internal/engine"
)

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	w := worker.New(c, engine.TaskQueue, worker.Options{})
	w.RegisterWorkflow(engine.JobWorkflow)
	w.RegisterActivity(&engine.Activities{})

	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker: %v", err)
	}
}
