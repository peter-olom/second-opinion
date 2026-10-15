package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// codex runs `codex exec` in a read-only sandbox, ignoring the user's config
// (hooks, MCP servers, profiles) and without persisting the session.
// Authentication still comes from CODEX_HOME. In a workspace it can run
// read-only commands there (rg, git log, ...); without one it runs in the
// empty scratch directory.
type codex struct{ opts Options }

func (c codex) Run(ctx context.Context, req Request) (string, error) {
	lastMessage := filepath.Join(req.Dir, "last-message.txt")
	args := []string{
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"--ignore-user-config",
		"--ignore-rules",
		"--sandbox", "read-only",
		"--color", "never",
		"--output-last-message", lastMessage,
		"--cd", req.cwd(),
		"-c", "developer_instructions=" + tomlString(req.System),
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if e := codexEffort(req.Effort); e != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(e))
	}

	if _, err := run(ctx, "codex", c.opts, req.cwd(), args, req.Prompt); err != nil {
		return "", err
	}
	out, err := os.ReadFile(lastMessage)
	if err != nil {
		return "", fmt.Errorf("codex: no final message: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// codexEffort passes levels through: current models support low to max, and
// `codex debug models` lists each model's levels. Codex has no "off", so the
// levels below low map to low.
func codexEffort(e string) string {
	switch e {
	case "off", "none", "minimal":
		return "low"
	}
	return e
}

// tomlString quotes s as a TOML basic string. JSON string escapes are a subset
// of TOML's, so a JSON-encoded string is valid TOML.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
