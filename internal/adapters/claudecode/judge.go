package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
)

// CLIJudge scores an attempt by shelling the Claude Code CLI with a judging
// prompt. Model selects the judge model (--model); running several
// CLIJudges with distinct models gives the gate model diversity within one
// vendor — a documented relaxation of ADR-0003's different-family rule
// until a second provider adapter exists.
type CLIJudge struct {
	Bin   string
	Model string
}

var _ ports.Judge = (*CLIJudge)(nil)

// verdictPayload is the strict JSON the judge prompt demands from the model.
type verdictPayload struct {
	Scores   map[string]int `json:"scores"`
	Findings []string       `json:"findings"`
}

func (j *CLIJudge) Judge(ctx context.Context, req ports.JudgeRequest) (ports.Judgment, error) {
	cmd := exec.CommandContext(ctx, j.Bin, "-p", judgePrompt(req), "--output-format", "json", "--model", j.Model)

	out, err := cmd.Output()
	if err != nil {
		return ports.Judgment{}, fmt.Errorf("judge %s run: %w", j.Model, err)
	}

	var env resultEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return ports.Judgment{}, fmt.Errorf("judge %s: parse claude output: %w", j.Model, err)
	}
	if env.IsError {
		return ports.Judgment{}, fmt.Errorf("judge %s reported error: %s", j.Model, env.Result)
	}

	var payload verdictPayload
	if err := json.Unmarshal([]byte(stripFences(env.Result)), &payload); err != nil {
		return ports.Judgment{}, fmt.Errorf("judge %s: verdict is not valid JSON: %w", j.Model, err)
	}

	return ports.Judgment{
		Verdict: gate.Verdict{
			Judge:         j.Model,
			RubricVersion: req.Rubric.Version,
			Scores:        payload.Scores,
			Findings:      payload.Findings,
		},
		CostUSD: env.TotalCostUSD,
	}, nil
}

// judgePrompt asks for scores on every axis. It shares the axis
// descriptions and the scale but never the pass thresholds: thresholds are
// the policy's business, and anchoring judges on them would bias scores
// toward the boundary.
func judgePrompt(req ports.JudgeRequest) string {
	var b strings.Builder
	b.WriteString("You are an independent code-review judge. Score the following change against each rubric axis on a scale of ")
	fmt.Fprintf(&b, "%d (worst) to %d (best).\n\nRubric axes:\n", gate.ScaleMin, gate.ScaleMax)
	for _, ax := range req.Rubric.Axes {
		fmt.Fprintf(&b, "- %s: %s\n", ax.Name, ax.Description)
	}
	b.WriteString("\nTicket:\n")
	b.WriteString(req.Ticket)
	b.WriteString("\n\nChange (diff):\n")
	b.WriteString(req.Diff)
	b.WriteString("\n\nReply with ONLY a JSON object, no markdown, no prose, exactly this shape:\n")
	b.WriteString(`{"scores":{"<axis>":<int>, ...},"findings":["<short finding>", ...]}`)
	b.WriteString("\nScore every axis listed above.")
	return b.String()
}

// stripFences removes a wrapping markdown code fence, which models add
// despite instructions often enough that parsing must tolerate it.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}
