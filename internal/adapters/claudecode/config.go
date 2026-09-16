package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Benja272/tollgate/internal/ports"
)

// agentConfig is the parsed shape of ports.RunSpec.AgentConfig (ADR-0006
// D1). It is intentionally the only place the engine's opaque config gets
// first-class fields — the engine itself never inspects it (ADR-0002).
// Unknown fields fail the parse: a typo in the config must never silently
// no-op.
type agentConfig struct {
	Model        string   `json:"model"`
	Tools        []string `json:"tools"`
	AllowedTools []string `json:"allowed_tools"`
}

// parseAgentConfig decodes cfg. An empty or literal-null cfg is not an
// error: it reports ok=false so buildArgs can fall back to the pre-change
// argument list (agent-run-config#Empty Config Compatibility).
func parseAgentConfig(cfg json.RawMessage) (cfgOut agentConfig, ok bool, err error) {
	trimmed := bytes.TrimSpace(cfg)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return agentConfig{}, false, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfgOut); err != nil {
		return agentConfig{}, false, fmt.Errorf("%w: %v", ports.ErrInvalidAgentConfig, err)
	}
	return cfgOut, true, nil
}

// buildArgs builds the CLI argument list for one Claude Code invocation
// (ADR-0006 D2). An empty/null cfg produces exactly today's pre-change
// argument list. A non-empty cfg additionally pins `--restricted
// --strict-mcp-config` (D3) and a non-interactive `dontAsk` permission mode,
// and forwards the requested model and tool allowlist, with `--allowedTools`
// always last so it can take a trailing variadic list.
//
// Every config value is validated before any process is spawned: empty
// values, and values starting with `-` (which a naive argv builder could let
// the CLI reinterpret as another flag — threat matrix: agent argument
// injection), are rejected as ports.ErrInvalidAgentConfig rather than
// forwarded to exec.CommandContext.
func buildArgs(prompt string, cfg json.RawMessage) ([]string, error) {
	base := []string{"-p", prompt, "--output-format", "json"}

	c, has, err := parseAgentConfig(cfg)
	if err != nil {
		return nil, err
	}
	if !has {
		return base, nil
	}

	if c.Model != "" {
		if err := rejectUnsafeValue(c.Model); err != nil {
			return nil, err
		}
	}
	for _, tool := range c.Tools {
		if err := rejectUnsafeValue(tool); err != nil {
			return nil, err
		}
	}
	for _, tool := range c.AllowedTools {
		if err := rejectUnsafeValue(tool); err != nil {
			return nil, err
		}
	}

	args := make([]string, 0, len(base)+len(c.Tools)+len(c.AllowedTools)+10)
	args = append(args, base...)
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	args = append(args, "--restricted", "--strict-mcp-config")
	if len(c.Tools) > 0 {
		args = append(args, "--tools", strings.Join(c.Tools, ","))
	}
	args = append(args, "--permission-mode", "dontAsk", "--permission-prompts", "none")
	if len(c.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, c.AllowedTools...)
	}
	return args, nil
}

// rejectUnsafeValue rejects an empty value or one that a CLI argument
// parser could misinterpret as a flag. Values are never sanitized (e.g. by
// stripping a leading dash) — only rejected, since silently rewriting a
// value could still change which flag the CLI parses next.
func rejectUnsafeValue(v string) error {
	if v == "" || strings.HasPrefix(v, "-") {
		return fmt.Errorf("%w: unsafe value %q", ports.ErrInvalidAgentConfig, v)
	}
	return nil
}
