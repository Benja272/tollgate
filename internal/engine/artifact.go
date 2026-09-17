package engine

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Benja272/tollgate/internal/workspace"
)

// ArtifactJobInput describes one artifact job: check out Repo at exactly
// SourceRef, overlay prepared files onto declared destination roots, run
// the agent, record its cost — no PR, no judges, no gate (ADR-0006).
type ArtifactJobInput struct {
	JobID     string
	PieceID   string
	Repo      string
	SourceRef string
	Prompt    string

	// AgentConfig is opaque to the engine (ADR-0002) and required non-empty:
	// an artifact job always names a model and, usually, a tool allowlist.
	AgentConfig json.RawMessage

	// DestinationRoots bounds every overlay destination; at least one is
	// required (ADR-0006 §4).
	DestinationRoots []string
	Overlays         []workspace.Overlay

	// AgentTimeoutMinutes bounds the agent run (its StartToClose timeout).
	// Zero means defaultArtifactAgentTimeout; at most maxArtifactAgentTimeout.
	AgentTimeoutMinutes int
}

// defaultArtifactAgentTimeout is sized for the render phase of a piece: an
// agent that drives ffmpeg over a few minutes of footage routinely runs past
// the ten minutes a PR-shape agent run gets, and a run cut short by the
// timeout is unmetered and never retried (ADR-0006 §8). An hour covers a
// render with room to spare while still bounding a hung run.
const defaultArtifactAgentTimeout = 60 * time.Minute

// maxArtifactAgentTimeout caps an operator-supplied timeout: a run is paid
// work, and a day is past any legitimate single phase.
const maxArtifactAgentTimeout = 24 * time.Hour

// ArtifactJobResult is what a completed artifact job reports back.
type ArtifactJobResult struct {
	Workspace string
	Model     string
	CostUSD   float64
	Output    string
}

// sourceRefPattern is exactly 40 hex characters (ADR-0006 §2): no
// trimming, so surrounding whitespace fails the match rather than being
// tolerated.
var sourceRefPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// artifactJobIDPattern bounds job_id to characters safe as a single path
// segment: CheckoutWorkspace builds
// "<WorkspaceRoot>/tollgate-artifact-<job_id>" from it directly.
var artifactJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validate is the pure precondition gate: it must fail — non-retryably,
// before any activity is scheduled — on any Input Validation MUST
// (ADR-0006 §2, §4). On success it returns in with SourceRef normalized to
// lowercase. It runs inside workflow code, so it never touches the
// filesystem and never parses the agent config: that format belongs to the
// adapter (ADR-0002), and ValidateAgentConfig checks it as the first
// activity, before any checkout.
func validate(in ArtifactJobInput) (ArtifactJobInput, error) {
	if !sourceRefPattern.MatchString(in.SourceRef) {
		return in, invalidInput("source_ref must be exactly 40 hexadecimal characters, got %q", in.SourceRef)
	}
	in.SourceRef = strings.ToLower(in.SourceRef)

	if !artifactJobIDPattern.MatchString(in.JobID) {
		return in, invalidInput("job_id %q does not match ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$", in.JobID)
	}

	if !filepath.IsAbs(in.Repo) {
		return in, invalidInput("repo must be an absolute path to a local clone, got %q", in.Repo)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return in, invalidInput("prompt must be non-empty")
	}
	if in.PieceID != "" && strings.TrimSpace(in.PieceID) == "" {
		return in, invalidInput("piece_id must be omitted or non-blank")
	}
	// Bounds are checked on the minutes themselves: multiplying first by
	// time.Minute can wrap a huge value into a small, valid-looking duration.
	if maxMinutes := int(maxArtifactAgentTimeout / time.Minute); in.AgentTimeoutMinutes < 0 || in.AgentTimeoutMinutes > maxMinutes {
		return in, invalidInput("agent_timeout_minutes must be between 0 and %d, got %d",
			maxMinutes, in.AgentTimeoutMinutes)
	}

	if isEmptyAgentConfig(in.AgentConfig) {
		return in, invalidInput("agent_config must be present and non-empty")
	}

	if err := workspace.ValidatePaths(in.DestinationRoots, in.Overlays); err != nil {
		return in, invalidInput("%v", err)
	}

	return in, nil
}

// isEmptyAgentConfig reports whether cfg is absent or the JSON literal
// null — the two shapes Input Validation rejects.
func isEmptyAgentConfig(cfg json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(cfg))
	return trimmed == "" || trimmed == "null"
}

// invalidInput builds the non-retryable error every validate() violation
// returns: retrying cannot fix a malformed input.
func invalidInput(format string, args ...interface{}) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), "InvalidInput", nil)
}

// ArtifactJobWorkflow orchestrates one artifact job: validate the agent
// config -> checkout -> overlay -> run agent -> record cost. It never invokes Ship, schedules no judge, and
// evaluates no gate (ADR-0006 "No PR, No Gate").
func ArtifactJobWorkflow(ctx workflow.Context, in ArtifactJobInput) (ArtifactJobResult, error) {
	in, err := validate(in)
	if err != nil {
		return ArtifactJobResult{}, err
	}

	var acts *Activities

	validateCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	if err := workflow.ExecuteActivity(validateCtx, acts.ValidateAgentConfig, in.AgentConfig).Get(validateCtx, nil); err != nil {
		return ArtifactJobResult{}, err
	}

	checkoutCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	var ws Workspace
	if err := workflow.ExecuteActivity(checkoutCtx, acts.CheckoutWorkspace, CheckoutInput{
		Repo: in.Repo, SourceRef: in.SourceRef, JobID: in.JobID,
	}).Get(checkoutCtx, &ws); err != nil {
		return ArtifactJobResult{}, err
	}

	// ApplyOverlay is its own activity (ADR-0006 §3): its failure must
	// never invoke — and never bill — the agent
	// (workspace-preparation#Overlay Failure Isolation).
	overlayCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	if err := workflow.ExecuteActivity(overlayCtx, acts.ApplyOverlay, OverlayInput{
		Workspace: ws, DestinationRoots: in.DestinationRoots, Overlays: in.Overlays,
	}).Get(overlayCtx, nil); err != nil {
		return ArtifactJobResult{}, err
	}

	agentTimeout := defaultArtifactAgentTimeout
	if in.AgentTimeoutMinutes > 0 {
		agentTimeout = time.Duration(in.AgentTimeoutMinutes) * time.Minute
	}
	agentCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: agentTimeout,
		HeartbeatTimeout:    5 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: runAgentMaxAttempts},
	})
	recordCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Minute})
	agent, err := runAgentAndRecord(agentCtx, recordCtx, RunAgentInput{
		JobID:       in.JobID,
		Workspace:   ws,
		Prompt:      in.Prompt,
		Attempt:     1,
		AgentConfig: in.AgentConfig,
	}, "agent", in.PieceID)
	if err != nil {
		return ArtifactJobResult{}, err
	}

	return ArtifactJobResult{
		Workspace: ws.Path,
		Model:     agent.Model,
		CostUSD:   agent.CostUSD,
		Output:    agent.Output,
	}, nil
}
