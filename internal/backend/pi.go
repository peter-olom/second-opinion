package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// pi runs pi in print mode with no extensions, skills, project-local resources,
// or session. In a workspace it gets the read-only tools and the project's
// context files; otherwise it has no tools at all.
type pi struct{ opts Options }

func (p pi) Run(ctx context.Context, req Request) (string, error) {
	// pi appends a discovered APPEND_SYSTEM.md (from ~/.pi/agent, or a
	// trusted project) only when no --append-system-prompt is given, so one
	// is always passed. pi reads the value as a file if a file by that name
	// exists, relative to its working directory (the repository, with repo
	// access), so it is an absolute path to a harmless file in the private
	// scratch folder rather than literal text a repo file could shadow.
	appendFile := filepath.Join(req.Dir, "append-system-prompt.md")
	if err := os.WriteFile(appendFile, []byte("\n"), 0o600); err != nil {
		return "", err
	}
	args := []string{
		"--print",
		"--no-session",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-approve",
		"--system-prompt", req.System,
		"--append-system-prompt", appendFile,
	}
	if req.Workspace != "" {
		args = append(args, "--tools", "read,grep,find,ls")
	} else {
		args = append(args, "--no-tools", "--no-context-files")
	}
	if req.Provider != "" {
		args = append(args, "--provider", req.Provider)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if e := piEffort(req.Effort); e != "" {
		args = append(args, "--thinking", e)
	}

	out, err := run(ctx, "pi", p.opts, req.cwd(), args, req.Prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// piEffort maps to pi's thinking levels: off, minimal, low, medium, high, xhigh, max.
func piEffort(e string) string {
	if e == "none" {
		return "off"
	}
	return e
}
