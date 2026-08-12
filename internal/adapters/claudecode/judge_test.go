package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Benja272/tollgate/internal/gate"
	"github.com/Benja272/tollgate/internal/ports"
)

func judgeRubric(t *testing.T) gate.Rubric {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rubric.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`name: test
axes:
  - name: correctness
    description: Does what the ticket asks.
    blocking: true
    min_score: 4
  - name: clarity
    description: Readable.
    blocking: false
    min_score: 3
`), 0o644))
	r, err := gate.LoadRubric(path)
	require.NoError(t, err)
	return r
}

// envelopeWith wraps an inner agent answer in the claude CLI result JSON.
func envelopeWith(result string) string {
	return `printf '{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.02,"session_id":"j1","result":%s}' '` + result + `'`
}

func TestCLIJudge_ParsesVerdict(t *testing.T) {
	bin := fakeClaude(t, envelopeWith(`"{\"scores\":{\"correctness\":4,\"clarity\":2},\"findings\":[\"naming could improve\"]}"`))
	j := &CLIJudge{Bin: bin, Model: "haiku"}
	rubric := judgeRubric(t)

	v, err := j.Judge(context.Background(), ports.JudgeRequest{Diff: "diff", Ticket: "ticket", Rubric: rubric})

	require.NoError(t, err)
	require.Equal(t, "haiku", v.Judge)
	require.Equal(t, rubric.Version, v.RubricVersion, "verdict must pin the rubric version it judged against")
	require.Equal(t, map[string]int{"correctness": 4, "clarity": 2}, v.Scores)
	require.Equal(t, []string{"naming could improve"}, v.Findings)
}

func TestCLIJudge_StripsMarkdownFences(t *testing.T) {
	bin := fakeClaude(t, envelopeWith(`"`+"```json\\n"+`{\"scores\":{\"correctness\":5,\"clarity\":5},\"findings\":[]}`+"\\n```"+`"`))
	j := &CLIJudge{Bin: bin, Model: "sonnet"}

	v, err := j.Judge(context.Background(), ports.JudgeRequest{Diff: "d", Ticket: "t", Rubric: judgeRubric(t)})

	require.NoError(t, err)
	require.Equal(t, 5, v.Scores["correctness"])
}

func TestCLIJudge_MalformedVerdictJSON_IsError(t *testing.T) {
	bin := fakeClaude(t, envelopeWith(`"the change looks great, 5 stars!"`))
	j := &CLIJudge{Bin: bin, Model: "haiku"}

	_, err := j.Judge(context.Background(), ports.JudgeRequest{Diff: "d", Ticket: "t", Rubric: judgeRubric(t)})

	require.Error(t, err, "prose instead of JSON must be a judge failure, never a silent verdict")
}

func TestCLIJudge_PassesModelFlag(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := fakeClaude(t, `echo "$@" > `+argsFile+`
`+envelopeWith(`"{\"scores\":{\"correctness\":4,\"clarity\":4},\"findings\":[]}"`))
	j := &CLIJudge{Bin: bin, Model: "opus"}

	_, err := j.Judge(context.Background(), ports.JudgeRequest{Diff: "d", Ticket: "t", Rubric: judgeRubric(t)})
	require.NoError(t, err)

	args, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	require.Contains(t, string(args), "--model opus")
}
