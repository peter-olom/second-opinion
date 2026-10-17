package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
		// Commands Codex runs see only core variables (HOME, PATH, …), not
		// the caller's API keys and tokens, plus the recursion guard.
		"-c", `shell_environment_policy.inherit="core"`,
		"-c", `shell_environment_policy.set={` + EnvGuard + `="1"}`,
		// Codex has no switch for skills, and --ignore-user-config leaves
		// them on. A one-token catalog budget drops every skill from what
		// the model sees (verified with Codex 0.159).
		"-c", "skills.max_context_tokens=1",
		// Don't load the repository's AGENTS.md, which could steer the
		// panelist. (~/.codex/AGENTS.md still loads; Codex has no switch.)
		"-c", "project_doc_max_bytes=0",
	}
	features, err := codexFeatures(ctx, c.opts)
	if err != nil {
		return "", err
	}
	args = append(args, codexLockdown(features, req.Workspace != "")...)
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if e := codexEffort(req.Effort); e != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(e))
	}

	if _, err := run(ctx, "codex", c.opts, req.cwd(), args, req.Prompt); err != nil {
		return "", err
	}
	f, err := os.Open(lastMessage)
	if err != nil {
		return "", fmt.Errorf("codex: no final message: %w", err)
	}
	defer f.Close()
	out, err := io.ReadAll(io.LimitReader(f, int64(maxStdout)+1))
	if err != nil {
		return "", fmt.Errorf("codex: reading final message: %w", err)
	}
	if len(out) > maxStdout {
		return "", fmt.Errorf("codex: output exceeded %d bytes", maxStdout)
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
}

// codexShellFeatures give the model read-only commands, which repo access
// needs. Some models (gpt-6.1-sol, for one) run commands through "code mode",
// which needs code_mode_host; others call the shell tool directly.
var codexShellFeatures = map[string]bool{"shell_tool": true, "unified_exec": true, "code_mode_host": true}

type featureCache struct {
	mu    sync.Mutex
	names []string
	// busy is held while one caller lists the features; others wait on it
	// or on their own context, whichever ends first.
	busy chan struct{}
}

var codexFeatureList featureCache

// codexFeatures lists every feature this Codex knows about, whatever its
// state. The state column is ignored: it reflects the user's config, which
// panelists run without. Only a successful list is cached, so one slow start
// does not fail every later Codex call. If the list is unavailable the
// panelist fails rather than run with Codex's tools unlocked: guessing names
// is no alternative, since Codex rejects a feature it does not know.
func codexFeatures(ctx context.Context, o Options) ([]string, error) {
	codexFeatureList.mu.Lock()
	if codexFeatureList.busy == nil {
		codexFeatureList.busy = make(chan struct{}, 1)
	}
	busy := codexFeatureList.busy
	codexFeatureList.mu.Unlock()
	select {
	case busy <- struct{}{}:
		defer func() { <-busy }()
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("codex: listing its features: %w", ErrTimeout)
		}
		return nil, fmt.Errorf("codex: listing its features: %w", ctx.Err())
	}
	// mu guards only the cache, never the listing, so a waiter can always
	// reach the select above.
	codexFeatureList.mu.Lock()
	cached := codexFeatureList.names
	codexFeatureList.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	env := os.Environ()
	for k, v := range o.Env {
		env = append(env, k+"="+v)
	}
	// A private folder, not the shared temp directory, where another user
	// could plant a .codex folder.
	scratch, err := os.MkdirTemp("", "so-codex-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	out, err := execCLI(ctx, Command("codex", o), []string{"features", "list"}, scratch, env, "")
	switch {
	case errors.Is(parent.Err(), context.DeadlineExceeded):
		// The panelist's own timeout ran out during discovery.
		return nil, fmt.Errorf("codex: listing its features: %w", ErrTimeout)
	case parent.Err() != nil:
		// The run was interrupted; keep that identity.
		return nil, fmt.Errorf("codex: listing its features: %w", parent.Err())
	case ctx.Err() != nil:
		return nil, errors.New("codex: `codex features list` did not finish within 30s; cannot disable its tools, so not running it")
	case err != nil:
		return nil, fmt.Errorf("codex: cannot list its features to disable its tools (%v); check that Codex and ~/.codex/config.toml work", err)
	}
	names := parseCodexFeatures(string(out))
	if len(names) == 0 {
		return nil, errors.New("codex: `codex features list` returned nothing so could parse; cannot disable its tools, so not running it")
	}
	codexFeatureList.mu.Lock()
	codexFeatureList.names = names
	codexFeatureList.mu.Unlock()
	return names, nil
}

// parseCodexFeatures reads "name  stage  true|false" lines.
func parseCodexFeatures(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 || (f[len(f)-1] != "true" && f[len(f)-1] != "false") {
			// An unknown line format could hide features the lockdown
			// should disable: fail closed rather than disable some.
			return nil
		}
		names = append(names, f[0])
	}
	return names
}

// codexLockdown disables every feature except the network ones, and the shell
// tools too unless the panelist has a workspace to read. It also turns off web
// search. The read-only sandbox, not this list, is what keeps commands from
// writing anything.
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
