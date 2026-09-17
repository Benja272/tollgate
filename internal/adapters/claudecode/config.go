package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/Benja272/tollgate/internal/ports"
)

// agentConfig is the parsed shape of ports.RunSpec.AgentConfig (ADR-0006
// §5). It is intentionally the only place the engine's opaque config gets
// first-class fields — the engine itself never inspects it (ADR-0002).
// Unknown fields fail the parse: a typo in the config must never silently
// no-op.
type agentConfig struct {
	Model string `json:"model"`
	// Tools is a pointer so `"tools": []` (no tools at all) stays distinct
	// from an absent key (the CLI's default tool set).
	Tools        *[]string `json:"tools"`
	AllowedTools []string  `json:"allowed_tools"`
}

// parseAgentConfig decodes and validates cfg. An empty or literal-null cfg
// is not an error: it reports ok=false so buildArgs can fall back to the
// minimal argument list (agent-run-config#Empty Config Compatibility).
func parseAgentConfig(cfg json.RawMessage) (cfgOut agentConfig, ok bool, err error) {
	trimmed := bytes.TrimSpace(cfg)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return agentConfig{}, false, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfgOut); err != nil {
		return agentConfig{}, false, invalidConfig("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return agentConfig{}, false, invalidConfig("trailing data after the config object")
	}

	if cfgOut.Model != "" {
		if err := checkModel(cfgOut.Model); err != nil {
			return agentConfig{}, false, err
		}
	}
	if cfgOut.Tools != nil {
		for _, tool := range *cfgOut.Tools {
			if err := checkToolName(tool); err != nil {
				return agentConfig{}, false, err
			}
		}
	}
	for _, rule := range cfgOut.AllowedTools {
		if err := checkAllowRule(rule); err != nil {
			return agentConfig{}, false, err
		}
	}
	return cfgOut, true, nil
}

func invalidConfig(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ports.ErrInvalidAgentConfig, fmt.Sprintf(format, args...))
}

// buildArgs builds the CLI argument list for one Claude Code invocation
// (ADR-0006 §5). An empty/null cfg produces the minimal list. A non-empty
// cfg additionally pins `--restricted --strict-mcp-config` and a
// non-interactive `dontAsk` permission mode, and forwards the requested
// model and tool allowlist.
//
// The prompt is always the single operand after a `--` terminator placed
// after every flag. The CLI takes the prompt as a positional argument, so a
// prompt starting with `-` would otherwise parse as an option (a prompt of
// `--dangerously-skip-permissions` would pass the bypass flag). The
// terminator also closes the variadic `--allowedTools`. Config values are
// validated before any process is spawned and are rejected, never
// rewritten.
func buildArgs(prompt string, cfg json.RawMessage) ([]string, error) {
	c, has, err := parseAgentConfig(cfg)
	if err != nil {
		return nil, err
	}

	args := []string{"-p", "--output-format", "json"}
	if has {
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		args = append(args, "--restricted", "--strict-mcp-config")
		if c.Tools != nil {
			args = append(args, "--tools", strings.Join(*c.Tools, ","))
		}
		args = append(args, "--permission-mode", "dontAsk", "--permission-prompts", "none")
		if len(c.AllowedTools) > 0 {
			args = append(args, "--allowedTools")
			args = append(args, c.AllowedTools...)
		}
	}
	return append(args, "--", prompt), nil
}

// checkModel accepts a model id or alias: non-empty, no leading dash, no
// whitespace or control characters.
func checkModel(v string) error {
	if strings.HasPrefix(v, "-") || strings.IndexFunc(v, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return invalidConfig("unsafe model %q", v)
	}
	return nil
}

// checkToolName accepts one --tools entry. Entries are joined with commas,
// so a comma inside one would smuggle in a second tool.
func checkToolName(v string) error {
	if v == "" || strings.HasPrefix(v, "-") || strings.IndexFunc(v, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return invalidConfig("unsafe tools entry %q", v)
	}
	return nil
}

// checkAllowRule accepts one permission rule such as `Read` or
// `Bash(git log *)`. The CLI splits rule lists on commas and whitespace
// outside parentheses, so either one there would turn a single entry into
// several rules; inside balanced parentheses both are part of the pattern.
func checkAllowRule(v string) error {
	if v == "" || strings.HasPrefix(v, "-") {
		return invalidConfig("unsafe allowed_tools entry %q", v)
	}
	depth := 0
	for _, r := range v {
		switch {
		case unicode.IsControl(r):
			return invalidConfig("allowed_tools entry %q contains a control character", v)
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth < 0 {
				return invalidConfig("allowed_tools entry %q has unbalanced parentheses", v)
			}
		case depth == 0 && (r == ',' || unicode.IsSpace(r)):
			return invalidConfig("allowed_tools entry %q has a separator outside parentheses", v)
		}
	}
	if depth != 0 {
		return invalidConfig("allowed_tools entry %q has unbalanced parentheses", v)
	}
	return nil
}
