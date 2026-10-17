package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// pi runs pi in print mode with no extensions, skills, project-local resources,
// or session. In a workspace it gets the read-only tools and the project's
// context files; otherwise it has no tools at all.
//
// pi migrates files in its working directory at startup. The one project-level
// migration in pi 1.3 renames .pi/commands to .pi/prompts when only the first
// exists; in such a repository pi is not run at all, so a review can never
// modify the repository.
type pi struct{ opts Options }

func (p pi) Run(ctx context.Context, req Request) (string, error) {
	if req.Workspace != "" && piWouldMigrate(req.Workspace) {
		return "", errors.New("pi: not run in this repository: pi would rename its .pi/commands to .pi/prompts at startup (a pi migration), which would modify the repository")
	}
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
	// Never load AGENTS.md context files: a reviewed repository's could
	// steer the panelist, and the user's own are not for panelists.
	args = append(args, "--no-context-files")
	if req.Workspace != "" {
		args = append(args, "--tools", "read,grep,find,ls")
	} else {
		args = append(args, "--no-tools")
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

// piWouldMigrate reports whether pi's startup migration would rename
// <repo>/.pi/commands to .pi/prompts.
func piWouldMigrate(repo string) bool {
	_, cmdErr := os.Lstat(filepath.Join(repo, ".pi", "commands"))
	_, promptsErr := os.Lstat(filepath.Join(repo, ".pi", "prompts"))
	return cmdErr == nil && os.IsNotExist(promptsErr)
}

// piEffort maps to pi's thinking levels: off, minimal, low, medium, high, xhigh, max.
func piEffort(e string) string {
	if e == "none" {
		return "off"
	}
	return e
}
