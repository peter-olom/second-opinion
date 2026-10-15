// Package gitctx gathers review context from a git repository: the diff against
// a base, plus the full current contents of every changed and untracked file.
package gitctx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	maxDiffBytes  = 1 << 20
	maxFileBytes  = 256 << 10
	maxFilesBytes = 1536 << 10
)

// Root returns the top-level directory of the git repository containing dir.
func Root(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Context is the rendered review context for a set of changes.
type Context struct {
	Text    string
	Summary string // e.g. "diff vs main (merge-base 1a2b3c4), 5 files"
}

// Diff renders the changes in the working tree relative to base. For a branch
// such as "main" the diff starts at the merge-base, so it shows this branch's
// work (committed or not) and none of main's later commits.
func Diff(dir, base string) (Context, error) {
	root, err := Root(dir)
	if err != nil {
		return Context{}, errors.New("--diff needs to run inside a git repository")
	}
	if _, err := git(root, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		if base == "HEAD" {
			return Context{}, errors.New("--diff: the repository has no commits yet; attach files with -f instead")
		}
		return Context{}, fmt.Errorf("--diff: unknown base %q", base)
	}
	from, baseDesc := base, base
	if mb, err := git(root, "merge-base", base, "HEAD"); err == nil {
		if mb = strings.TrimSpace(mb); mb != "" {
			from = mb
			if base != "HEAD" {
				baseDesc = fmt.Sprintf("%s (merge-base %.7s)", base, mb)
			}
		}
	}

	diff, err := git(root, "diff", "--no-color", "--no-ext-diff", from)
	if err != nil {
		return Context{}, err
	}
	changed, err := gitPaths(root, "diff", "--name-only", "--diff-filter=d", "-z", from)
	if err != nil {
		return Context{}, err
	}
	untracked, err := gitPaths(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Context{}, err
	}
	if strings.TrimSpace(diff) == "" && len(untracked) == 0 {
		return Context{}, fmt.Errorf("--diff: no changes against %s", baseDesc)
	}

	var b strings.Builder
	if len(diff) > maxDiffBytes {
		diff = diff[:maxDiffBytes] + "\n[diff truncated]"
	}
	fmt.Fprintf(&b, "<git-diff base=%q>\n%s\n</git-diff>", baseDesc, strings.TrimRight(diff, "\n"))

	var omitted []string
	total := 0
	attach := func(path, status string) {
		data, err := os.ReadFile(filepath.Join(root, path))
		switch {
		case err != nil:
			omitted = append(omitted, path+" (unreadable)")
		case bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0:
			omitted = append(omitted, path+" (binary)")
		case len(data) > maxFileBytes:
			omitted = append(omitted, path+" (too large)")
		case total+len(data) > maxFilesBytes:
			omitted = append(omitted, path+" (attachment budget exceeded)")
		default:
			total += len(data)
			fmt.Fprintf(&b, "\n\n<file path=%q status=%q>\n%s\n</file>", path, status, strings.TrimRight(string(data), "\n"))
		}
	}
	for _, p := range changed {
		attach(p, "changed")
	}
	for _, p := range untracked {
		attach(p, "new, untracked")
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "\n\nChanged files not attached: %s", strings.Join(omitted, ", "))
	}

	n := len(changed) + len(untracked)
	return Context{
		Text:    b.String(),
		Summary: fmt.Sprintf("diff vs %s, %d file%s", baseDesc, n, plural(n)),
	}, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

func gitPaths(dir string, args ...string) ([]string, error) {
	out, err := git(dir, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
