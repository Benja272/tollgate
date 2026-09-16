package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Benja272/tollgate/internal/ports"
)

func TestBuildArgs_EmptyConfig_MatchesPreChangeArgs(t *testing.T) {
	for name, cfg := range map[string]json.RawMessage{
		"nil":   nil,
		"empty": json.RawMessage(``),
		"null":  json.RawMessage(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			args, err := buildArgs("implement the ticket", cfg)
			require.NoError(t, err)
			assert.Equal(t, []string{"-p", "implement the ticket", "--output-format", "json"}, args)
		})
	}
}

func TestBuildArgs_FullConfig_IncludesModelToolsAllowlist(t *testing.T) {
	cfg := json.RawMessage(`{"model":"sonnet","tools":["Read","Write"],"allowed_tools":["Read","Write"]}`)

	args, err := buildArgs("do the thing", cfg)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"-p", "do the thing", "--output-format", "json",
		"--model", "sonnet",
		"--restricted", "--strict-mcp-config",
		"--tools", "Read,Write",
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--allowedTools", "Read", "Write",
	}, args)
}

func TestBuildArgs_AllowedToolsGoesLast(t *testing.T) {
	cfg := json.RawMessage(`{"model":"sonnet","allowed_tools":["Bash(echo *)"]}`)

	args, err := buildArgs("p", cfg)

	require.NoError(t, err)
	require.NotEmpty(t, args)
	assert.Equal(t, "--allowedTools", args[len(args)-2])
	assert.Equal(t, "Bash(echo *)", args[len(args)-1])
}

func TestBuildArgs_NeverEmitsBypassOrDangerousFlag(t *testing.T) {
	cfg := json.RawMessage(`{"model":"sonnet","tools":["Bash"],"allowed_tools":["Bash(*)"]}`)

	args, err := buildArgs("p", cfg)

	require.NoError(t, err)
	for _, a := range args {
		assert.NotContains(t, a, "bypass")
		assert.NotContains(t, a, "dangerously")
	}
}

func TestBuildArgs_UnknownField_RejectedBeforeSpawning(t *testing.T) {
	cfg := json.RawMessage(`{"model":"sonnet","unknown_field":"x"}`)

	_, err := buildArgs("p", cfg)

	require.Error(t, err)
	assert.ErrorIs(t, err, ports.ErrInvalidAgentConfig)
}

func TestBuildArgs_ValueStartingWithDash_Rejected(t *testing.T) {
	cases := map[string]json.RawMessage{
		"model":         json.RawMessage(`{"model":"-model"}`),
		"tools entry":   json.RawMessage(`{"model":"sonnet","tools":["-rule"]}`),
		"allowed entry": json.RawMessage(`{"model":"sonnet","allowed_tools":["-rule"]}`),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildArgs("p", cfg)
			require.Error(t, err)
			assert.ErrorIs(t, err, ports.ErrInvalidAgentConfig)
		})
	}
}

func TestBuildArgs_AlwaysIncludesRestricted(t *testing.T) {
	cfg := json.RawMessage(`{"model":"haiku"}`)

	args, err := buildArgs("p", cfg)

	require.NoError(t, err)
	assert.Contains(t, args, "--restricted")
	assert.Contains(t, args, "--strict-mcp-config")
}
