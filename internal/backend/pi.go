package backend

import (
	"context"
	"strings"
)

// pi runs pi in print mode with no extensions, skills, project-local resources,
// or session. In a workspace it gets the read-only tools and the project's
// context files; otherwise it has no tools at all.
type pi struct{ opts Options }

func (p pi) Run(ctx context.Context, req Request) (string, error) {
	args := []string{
		"--print",
		"--no-session",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-approve",
		"--system-prompt", req.System,
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
