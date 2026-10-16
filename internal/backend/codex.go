package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// codex runs `codex exec` in a read-only sandbox, ignoring the user's config
// (hooks, MCP servers, profiles) and without persisting the session.
// Authentication still comes from CODEX_HOME.
//
// Codex enables many tools by default, including the user's connected apps
// (Gmail, GitHub, Jira, ...), browser and computer use, and sub-agents. Every
// feature Codex knows about is disabled except a few that only affect
// networking, and so is web search. Without a workspace the shell tools are
// disabled too; with one they stay on so the model can read the repository.
//
// Codex has no switch that guarantees no tools at all: in testing, a command
// path occasionally reappeared with everything disabled. The read-only
// sandbox remains the hard boundary: nothing can be written, but a Codex
// panelist may still be able to read files the user can read.
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
	args = append(args, codexLockdown(codexFeatures(ctx, c.opts), req.Workspace != "")...)
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

// codexNetworkFeatures only affect how Codex talks to its API, never what the
// model can do, so they stay as they are.
var codexNetworkFeatures = map[string]bool{
	"enable_request_compression":   true,
	"unbounded_connection_retries": true,
	"system_proxy_fallback":        true,
	"content_item_kinds":           true,
	"compaction_image_budget":      true,
	"auth_elicitation":             true,
}

// codexShellFeatures give the model read-only commands, which repo access
// needs. Some models (gpt-6.1-sol, for one) run commands through "code mode",
// which needs code_mode_host; others call the shell tool directly.
var codexShellFeatures = map[string]bool{"shell_tool": true, "unified_exec": true, "code_mode_host": true}

// codexKnownFeatures is used if `codex features list` is unavailable: the
// features known to add tools or act outside the sandbox.
var codexKnownFeatures = []string{
	"apps", "plugins", "remote_plugin", "plugin_sharing", "browser_use", "browser_use_external",
	"browser_use_full_cdp_access", "computer_use", "in_app_browser", "multi_agent", "image_generation",
	"memories", "hooks", "goals", "view_image", "skill_search", "tool_suggest", "code_mode_host",
	"realtime_conversation", "worktrees", "workspace_dependencies", "shell_tool", "unified_exec",
}

var codexFeatureList struct {
	once  sync.Once
	names []string
}

// codexFeatures lists every feature this Codex knows about, whatever its
// state. The state column is ignored: it reflects the user's config, which
// panelists run without.
func codexFeatures(ctx context.Context, o Options) []string {
	codexFeatureList.once.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, Command("codex", o), "features", "list").Output()
		if err == nil {
			codexFeatureList.names = parseCodexFeatures(string(out))
		}
		if len(codexFeatureList.names) == 0 {
			codexFeatureList.names = codexKnownFeatures
		}
	})
	return codexFeatureList.names
}

// parseCodexFeatures reads "name  stage  true|false" lines.
func parseCodexFeatures(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && (f[len(f)-1] == "true" || f[len(f)-1] == "false") {
			names = append(names, f[0])
		}
	}
	return names
}

// codexLockdown disables every feature except the network ones, and the shell
// tools too unless the panelist has a workspace to read. It also turns off web
// search, so nothing a panelist does reaches beyond the model provider.
func codexLockdown(features []string, workspace bool) []string {
	var args []string
	for _, f := range features {
		if codexNetworkFeatures[f] || (workspace && codexShellFeatures[f]) {
			continue
		}
		args = append(args, "--disable", f)
	}
	return append(args, "-c", `web_search="disabled"`)
}

// tomlString quotes s as a TOML basic string. JSON string escapes are a subset
// of TOML's, so a JSON-encoded string is valid TOML.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
