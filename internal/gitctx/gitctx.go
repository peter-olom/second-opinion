// Package gitctx gathers review context from a git repository: the diff against
// a base, plus the full current contents of every changed and untracked file.
//
// Everything gathered is sent to model providers, so collection is
// conservative: only regular files inside the repository are attached
// (symlinks are never followed), git's textconv and external diff drivers are
// off, and every read is bounded.
package gitctx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Limits on what --diff attaches. Variables so tests can lower them.
var (
	maxDiffBytes  = 1 << 20    // the git diff itself
	maxFileBytes  = 256 << 10  // any one attached file
	maxFilesBytes = 1536 << 10 // all attached files together
	maxPathsBytes = 1 << 20    // the list of changed and untracked paths
	maxOmitted    = 20         // files listed by name as not attached
)

// Root returns the top-level directory of the git repository containing dir.
func Root(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _, err := git(ctx, dir, 64<<10, "rev-parse", "--show-toplevel")
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
func Diff(ctx context.Context, dir, base string) (Context, error) {
	root, err := Root(dir)
	if err != nil {
		return Context{}, errors.New("--diff needs to run inside a git repository")
	}
	if _, _, err := git(ctx, root, 64<<10, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		if ctx.Err() != nil {
			return Context{}, ctx.Err()
		}
		if base == "HEAD" {
			return Context{}, errors.New("--diff: the repository has no commits yet; attach files with -f instead")
		}
		return Context{}, fmt.Errorf("--diff: unknown base %q", base)
	}
	from, baseDesc := base, base
	if mb, _, err := git(ctx, root, 64<<10, "merge-base", base, "HEAD"); err == nil {
		if mb = strings.TrimSpace(mb); mb != "" {
			from = mb
			if base != "HEAD" {
				baseDesc = fmt.Sprintf("%s (merge-base %.7s)", base, mb)
			}
		}
	}

	// --no-textconv and --no-ext-diff keep configured drivers (which might
	// decrypt secrets or run arbitrary commands) out of what is sent.
	diff, diffTruncated, err := git(ctx, root, maxDiffBytes, "diff", "--no-color", "--no-ext-diff", "--no-textconv", from)
	if err != nil {
		return Context{}, err
	}
	changed, err := gitPaths(ctx, root, "diff", "--name-only", "--diff-filter=d", "-z", from)
	if err != nil {
		return Context{}, err
	}
	untracked, err := gitPaths(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Context{}, err
	}
	if strings.TrimSpace(diff) == "" && len(untracked) == 0 {
		return Context{}, fmt.Errorf("--diff: no changes against %s", baseDesc)
	}

	var b strings.Builder
	if diffTruncated {
		diff += "\n[diff truncated]"
	}
	fmt.Fprintf(&b, "<git-diff base=%q>\n%s\n</git-diff>", baseDesc, strings.TrimRight(diff, "\n"))

	var omitted []string
	total := 0
	attach := func(path, status string) {
		if total >= maxFilesBytes {
			omitted = append(omitted, path+" (attachment budget exceeded)")
			return
		}
		data, why := readAttachment(filepath.Join(root, path), min(maxFileBytes, maxFilesBytes-total))
		if why != "" {
			omitted = append(omitted, path+" ("+why+")")
			return
		}
		total += len(data)
		fmt.Fprintf(&b, "\n\n<file path=%q status=%q>\n%s\n</file>", path, status, strings.TrimRight(string(data), "\n"))
	}
	for _, p := range changed {
		attach(p, "changed")
	}
	for _, p := range untracked {
		attach(p, "new, untracked")
	}
	if len(omitted) > 0 {
		listed := omitted[:min(len(omitted), maxOmitted)]
		fmt.Fprintf(&b, "\n\nChanged files not attached: %s", strings.Join(listed, ", "))
		if rest := len(omitted) - len(listed); rest > 0 {
			fmt.Fprintf(&b, ", and %d more", rest)
		}
	}

	n := len(changed) + len(untracked)
	return Context{
		Text:    b.String(),
		Summary: fmt.Sprintf("diff vs %s, %d file%s", baseDesc, n, plural(n)),
	}, nil
}

// readAttachment reads a file for attaching, or explains why it is not
// attached. Only regular files are read, without following symlinks, and
// never more than limit bytes.
func readAttachment(path string, limit int) ([]byte, string) {
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		return nil, "unreadable"
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, "symlink, not followed"
	case !fi.Mode().IsRegular():
		return nil, "not a regular file"
	case fi.Size() > int64(maxFileBytes):
		return nil, "too large"
	case fi.Size() > int64(limit):
		return nil, "attachment budget exceeded"
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, "unreadable"
	}
	defer f.Close()
	// The file could have been swapped for a symlink or device between the
	// Lstat and the open; check that what was opened is the file inspected.
	if ofi, err := f.Stat(); err != nil || !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) {
		return nil, "changed while reading"
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	switch {
	case err != nil:
		return nil, "unreadable"
	case len(data) > limit:
		return nil, "too large"
	case bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0:
		return nil, "binary"
	}
	return data, ""
}

// git runs a git command and returns at most limit bytes of its output,
// reporting whether there was more; git is stopped once the limit is reached.
// fsmonitor hooks are disabled, and git is told not to take optional locks.
func git(ctx context.Context, dir string, limit int, args ...string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr limitedBuffer
	stderr.limit = 64 << 10
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	if err := cmd.Start(); err != nil {
		return "", false, err
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, int64(limit)+1))
	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return "", false, ctx.Err()
	case truncated:
		return string(out), true, nil
	case readErr != nil:
		return "", false, readErr
	case waitErr != nil:
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", false, fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", false, fmt.Errorf("git %s: %w", args[0], waitErr)
	}
	return string(out), false, nil
}

func gitPaths(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, truncated, err := git(ctx, dir, maxPathsBytes, args...)
	if err != nil {
		return nil, err
	}
	if truncated {
		// Drop the final, possibly partial, path.
		if i := strings.LastIndexByte(out, 0); i >= 0 {
			out = out[:i]
		}
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// limitedBuffer keeps the first limit bytes written to it.
type limitedBuffer struct {
	buf   []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return string(b.buf) }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
