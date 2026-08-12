package engine

import (
	"context"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/Benja272/tollgate/internal/ports"
)

// defaultHeartbeatInterval must stay well under the RunAgent
// HeartbeatTimeout (5s) so a single delayed beat never reads as a dead
// worker.
const defaultHeartbeatInterval = 2 * time.Second

// Activities holds every side-effecting step of the job pipeline. It is the
// engine's dependency-injection point: fields are ports, wired to concrete
// adapters at worker startup.
type Activities struct {
	Agent ports.AgentRunner

	// HeartbeatInterval overrides the agent-run heartbeat cadence; zero
	// means defaultHeartbeatInterval. Tests shorten it.
	HeartbeatInterval time.Duration

	// heartbeat is the beat sink, injectable by tests; nil means
	// activity.RecordHeartbeat (the SDK test environment batches heartbeats,
	// so counting real ones is not observable there).
	heartbeat func(ctx context.Context)
}

func (a *Activities) beat(ctx context.Context) {
	if a.heartbeat != nil {
		a.heartbeat(ctx)
		return
	}
	activity.RecordHeartbeat(ctx)
}

func (a *Activities) Prepare(ctx context.Context, in JobInput) (Workspace, error) {
	panic("not implemented")
}

// RunAgent delegates to the AgentRunner port while heartbeating so the
// server can detect a dead worker mid-run instead of waiting out the
// activity timeout.
func (a *Activities) RunAgent(ctx context.Context, ws Workspace) (AgentResult, error) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(a.heartbeatInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.beat(ctx)
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	res, err := a.Agent.Run(ctx, ports.RunSpec{WorkspacePath: ws.Path})
	if err != nil {
		return AgentResult{}, err
	}
	return AgentResult{CostUSD: res.CostUSD}, nil
}

func (a *Activities) Judge(ctx context.Context, ws Workspace) (JudgeReport, error) {
	panic("not implemented")
}

func (a *Activities) DecideGate(ctx context.Context, report JudgeReport) (GateDecision, error) {
	panic("not implemented")
}

func (a *Activities) Ship(ctx context.Context, ws Workspace) (ShipResult, error) {
	panic("not implemented")
}

func (a *Activities) heartbeatInterval() time.Duration {
	if a.HeartbeatInterval > 0 {
		return a.HeartbeatInterval
	}
	return defaultHeartbeatInterval
}
