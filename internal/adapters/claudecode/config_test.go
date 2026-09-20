package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Benja272/tollgate/internal/ports"
)

// The PR shape's argv changed deliberately in the post-archive review: the
// prompt moved behind a "--" terminator so a prompt starting with "-" can
// never parse as a flag (ADR-0006 §5). The set of flags is unchanged.
func TestBuildArgs_EmptyConfig_MinimalArgsWithTerminatedPrompt(t *testing.T) {
	for name, cfg := range map[string]json.RawMessage{
		"nil":   nil,
		"empty": json.RawMessage(``),
		"null":  json.RawMessage(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			args, err := buildArgs("implement the ticket", cfg)
			require.NoError(t, err)
			assert.Equal(t, []string{"-p", "--output-format", "json", "--", "implement the ticket"}, args)
		})
	}
}

func TestBuildArgs_FullConfig_IncludesModelToolsAllowlist(t *testing.T) {
	cfg := json.RawMessage(`{"model":"sonnet","tools":["Read","Write"],"allowed_tools":["Read","Write"]}`)

	args, err := buildArgs("do the thing", cfg)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"-p", "--output-format", "json",
		"--model", "sonnet",
		"--restricted", "--strict-mcp-config",
		"--tools", "Read,Write",
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--allowedTools", "Read", "Write",
		"--", "do the thing",
	}, args)
}

func TestBuildArgs_AllowedToolsIsTheLastFlagBeforeTerminator(t *testing.T) {
	cfg := json.RawMessage(`{"model":"sonnet","allowed_tools":["Bash(echo *)"]}`)

	args, err := buildArgs("p", cfg)

	require.NoError(t, err)
	require.GreaterOrEqual(t, len(args), 4)
	assert.Equal(t, []string{"--allowedTools", "Bash(echo *)", "--", "p"}, args[len(args)-4:])
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

func TestBuildArgs_PromptStartingWithDash_FollowsTerminator(t *testing.T) {
	for _, prompt := range []string{"--version", "--dangerously-skip-permissions", "- item one\n- item two", "-p"} {
		for name, cfg := range map[string]json.RawMessage{
			"empty config": nil,
			"full config":  json.RawMessage(`{"model":"haiku","tools":["Read"],"allowed_tools":["Read"]}`),
		} {
			t.Run(name+"/"+prompt, func(t *testing.T) {
				args, err := buildArgs(prompt, cfg)
				require.NoError(t, err, "a prompt starting with a dash is legitimate and must not be rejected")
				requireSafeArgv(t, args, prompt)
			})
		}
	}
}

func TestBuildArgs_EmptyToolsList_EmitsEmptyToolsFlag(t *testing.T) {
	args, err := buildArgs("p", json.RawMessage(`{"model":"haiku","tools":[]}`))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"-p", "--output-format", "json",
		"--model", "haiku",
		"--restricted", "--strict-mcp-config",
		"--tools", "",
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--", "p",
	}, args, `"tools": [] means no tools at all, not the default tool set`)

	absent, err := buildArgs("p", json.RawMessage(`{"model":"haiku"}`))
	require.NoError(t, err)
	assert.NotContains(t, absent, "--tools", "an absent tools key keeps the CLI default")
}

func TestParseAgentConfig_RejectsMalformedValues(t *testing.T) {
	cases := map[string]string{
		"trailing data":                  `{"model":"haiku"} {"model":"opus"}`,
		"trailing garbage":               `{"model":"haiku"}x`,
		"comma inside a tools entry":     `{"model":"haiku","tools":["Read,Bash"]}`,
		"control char in a tools entry":  "{\"model\":\"haiku\",\"tools\":[\"Read\\u0000\"]}",
		"newline in a tools entry":       `{"model":"haiku","tools":["Read\nBash"]}`,
		"empty tools entry":              `{"model":"haiku","tools":[""]}`,
		"comma outside parentheses":      `{"model":"haiku","allowed_tools":["Read,Bash"]}`,
		"whitespace outside parentheses": `{"model":"haiku","allowed_tools":["Read Bash"]}`,
		"control char in allowed entry":  "{\"model\":\"haiku\",\"allowed_tools\":[\"Bash(echo \\u0007)\"]}",
		"unbalanced parentheses":         `{"model":"haiku","allowed_tools":["Bash(echo *"]}`,
		"whitespace in model":            `{"model":"haiku --verbose"}`,
		"control char in model":          "{\"model\":\"haiku\\u001b\"}",
		"not an object":                  `["haiku"]`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildArgs("p", json.RawMessage(cfg))
			require.ErrorIs(t, err, ports.ErrInvalidAgentConfig)
		})
	}
}

func TestParseAgentConfig_AcceptsRulesWithSpacesAndCommasInsideParentheses(t *testing.T) {
	args, err := buildArgs("p", json.RawMessage(`{"model":"haiku","allowed_tools":["Bash(git log --oneline, -n 5)","Read"]}`))
	require.NoError(t, err)
	assert.Contains(t, args, "Bash(git log --oneline, -n 5)")
}

func TestRunner_ValidateConfig(t *testing.T) {
	r := &Runner{Bin: "unused"}
	artifact := ports.AgentConfigRequirements{RequireModel: true}

	require.NoError(t, r.ValidateConfig(json.RawMessage(`{"model":"haiku","allowed_tools":["Read"]}`), artifact))
	require.NoError(t, r.ValidateConfig(json.RawMessage(`{"allowed_tools":["Read"]}`), ports.AgentConfigRequirements{}),
		"without the requirement a model-less config is valid")

	for name, cfg := range map[string]string{
		"model missing": `{"allowed_tools":["Read"]}`,
		"model empty":   `{"model":""}`,
		"null config":   `null`,
		"unknown field": `{"model":"haiku","bogus":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, r.ValidateConfig(json.RawMessage(cfg), artifact), ports.ErrInvalidAgentConfig)
		})
	}
}

func TestRunner_ShutdownBound_IsTheWaitDelay(t *testing.T) {
	assert.Equal(t, defaultWaitDelay, (&Runner{}).ShutdownBound())
	assert.Equal(t, 3*time.Second, (&Runner{WaitDelay: 3 * time.Second}).ShutdownBound())
}

// flagArity is every flag buildArgs may emit and how many values it takes;
// -1 marks the variadic --allowedTools, which runs until the terminator.
var flagArity = map[string]int{
	"-p": 0, "--output-format": 1, "--model": 1, "--restricted": 0,
	"--strict-mcp-config": 0, "--tools": 1, "--permission-mode": 1,
	"--permission-prompts": 1, "--allowedTools": -1,
}

// requireSafeArgv parses args with the exact grammar the CLI sees and
// asserts: only known flags appear, no value can be read as a flag, the
// permission mode is never a bypass, and the prompt is the single operand
// after the "--" terminator.
func requireSafeArgv(t *testing.T, args []string, prompt string) {
	t.Helper()
	require.GreaterOrEqual(t, len(args), 2)
	require.Equal(t, "--", args[len(args)-2], "the prompt must follow a terminator: %q", args)
	require.Equal(t, prompt, args[len(args)-1])

	flags := args[:len(args)-2]
	for i := 0; i < len(flags); i++ {
		flag := flags[i]
		arity, known := flagArity[flag]
		require.True(t, known, "unexpected flag %q in %q", flag, args)
		require.NotContains(t, strings.ToLower(flag), "dangerously")
		if arity == -1 {
			for _, v := range flags[i+1:] {
				require.False(t, strings.HasPrefix(v, "-"), "allowedTools value %q would parse as a flag", v)
			}
			return
		}
		for j := 0; j < arity; j++ {
			i++
			require.Less(t, i, len(flags), "flag %q is missing its value", flag)
			v := flags[i]
			require.False(t, strings.HasPrefix(v, "-"), "value %q of %q would parse as a flag", v, flag)
			if flag == "--permission-mode" {
				require.Equal(t, "dontAsk", v)
			}
		}
	}
}

func FuzzBuildArgs_NoPromptOrValueEverParsesAsAFlag(f *testing.F) {
	f.Add("--version", `{"model":"haiku","tools":["Read"],"allowed_tools":["Read"]}`)
	f.Add("--dangerously-skip-permissions", ``)
	f.Add("-p", `{"model":"sonnet","allowed_tools":["Bash(echo *)","--permission-mode"]}`)
	f.Add("hi", `{"model":"--dangerously-skip-permissions"}`)
	f.Add("hi", `{"model":"haiku","tools":[]}`)
	f.Add("hi", `null`)
	f.Fuzz(func(t *testing.T, prompt, cfg string) {
		args, err := buildArgs(prompt, json.RawMessage(cfg))
		if err != nil {
			require.ErrorIs(t, err, ports.ErrInvalidAgentConfig)
			return
		}
		requireSafeArgv(t, args, prompt)
	})
}
