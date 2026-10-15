package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// claude runs Claude Code in print mode with no MCP servers, settings, or session.
// In a workspace it gets the read-only file tools, and Claude Code denies reads
// outside the working directory.
type claude struct{ opts Options }

func (c claude) Run(ctx context.Context, req Request) (string, error) {
	tools := ""
	if req.Workspace != "" {
		tools = "Read,Grep,Glob"
	}
	args := []string{
		"--print",
		"--output-format", "json",
		"--tools", tools,
		"--strict-mcp-config",
		"--setting-sources", "",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--system-prompt", req.System,
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if e := claudeEffort(req.Effort); e != "" {
		args = append(args, "--effort", e)
	}

	out, err := run(ctx, "claude", c.opts, req.cwd(), args, req.Prompt)
	if err != nil {
		return "", err
	}
	var res struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("claude: unexpected output: %s", truncate(out, 500))
	}
	if res.IsError {
		return "", fmt.Errorf("claude: %s: %s", res.Subtype, res.Result)
	}
	return res.Result, nil
}

// claudeEffort maps to Claude Code's levels: low, medium, high, xhigh, max.
func claudeEffort(e string) string {
	switch e {
	case "off", "none", "minimal":
		return "low"
	}
	return e
}
