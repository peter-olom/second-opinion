// Package gitctx gathers review context from a git repository: the diff against
// a base, plus the full current contents of every changed and untracked file.
//
// Everything gathered is sent to model providers, so collection is
// conservative: only regular files inside the repository are attached
// (symlinks are never followed), git's textconv and external diff drivers are
// off, and every read is bounded.
package gitctx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Limits on what --diff attaches. Variables so tests can lower them.
var (
	maxDiffBytes    = 1 << 20    // the git diff itself, after filtered files are dropped
	maxRawDiffBytes = 64 << 20   // how much raw diff to read looking for that
	maxFileBytes    = 256 << 10  // any one attached file
	maxFilesBytes   = 1536 << 10 // all attached files together
	maxPathsBytes   = 1 << 20    // the list of changed and untracked paths
	maxOmitted      = 20         // files listed by name as not attached
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

	// Every path in the diff, deletions included, and the untracked files.
	// A truncated list would let files escape the filter check below, so it
	// is an error rather than a partial answer.
	// One listing gives every path and its status, so the attachments, the
	// filter check, and the diff allowlist all agree on the same set.
	statuses, err := gitPaths(ctx, root, "diff", "--name-status", "--no-renames", "-z", from)
	if err != nil {
		return Context{}, err
	}
	if len(statuses)%2 != 0 {
		return Context{}, errors.New("--diff: could not read git's list of changed files")
	}
	var inDiff, changed []string
	for i := 0; i < len(statuses); i += 2 {
		status, path := statuses[i], statuses[i+1]
		inDiff = append(inDiff, path)
		if !strings.HasPrefix(status, "D") {
			changed = append(changed, path)
		}
	}
	untracked, err := gitPaths(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Context{}, err
	}
	// Files under a git filter (git-crypt, transcrypt, sops, LFS, ...) can be
	// plaintext in the working tree while the repository stores ciphertext.
	// They are left out of both the diff and the attachments.
	filtered, err := filteredPaths(ctx, root, append(inDiff[:len(inDiff):len(inDiff)], untracked...))
	if err != nil {
		return Context{}, err
	}
	// --no-textconv and --no-ext-diff keep configured drivers (which might
	// decrypt secrets or run arbitrary commands) out of what is sent.
	// Filtered files are dropped from the output here, rather than with one
	// pathspec each, which could overflow the command line.
	// The header prefixes are pinned, since diff.mnemonicPrefix or
	// diff.noprefix in the user's config would change them. More than the
	// limit is read so excluded files do not use up the budget; the limit
	// applies to what is left.
	checked := map[string]bool{}
	for _, p := range inDiff {
		checked[p] = true
	}
	sections := &sectionFilter{checked: checked, drop: filtered}
	var kept strings.Builder
	diffTruncated, err := gitLines(ctx, root, maxRawDiffBytes, func(line string) bool {
		if sections.keepLine(line) {
			if kept.Len()+len(line) > maxDiffBytes {
				return false
			}
			kept.WriteString(line)
		}
		return true
	}, "-c", "core.quotePath=false", "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false",
		"diff", "--src-prefix=a/", "--dst-prefix=b/", "--submodule=short",
		"--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", from)
	if err != nil {
		return Context{}, err
	}
	diff, unparsed := kept.String(), sections.unparsed
	if len(inDiff) == 0 && len(untracked) == 0 {
		return Context{}, fmt.Errorf("--diff: no changes against %s", baseDesc)
	}

	var b strings.Builder
	if diffTruncated {
		diff += "\n[diff truncated]"
	}
	if unparsed > 0 {
		diff += fmt.Sprintf("\n[%d file section(s) left out: unrecognised diff header]", unparsed)
	}
	fmt.Fprintf(&b, "<git-diff base=%q>\n%s\n</git-diff>", baseDesc, strings.TrimRight(diff, "\n"))

	var omitted []string
	total := 0
	// Filtered files that were deleted never reach attach; note them too.
	isChanged := map[string]bool{}
	for _, p := range changed {
		isChanged[p] = true
	}
	for _, p := range inDiff {
		if filtered[p] && !isChanged[p] {
			omitted = append(omitted, p+" (deleted; git filter, may be encrypted)")
		}
	}
	attach := func(path, status string) {
		if filtered[path] {
			omitted = append(omitted, path+" (git filter; may be encrypted)")
			return
		}
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
	cmd.Env = append(gitEnv(), "GIT_OPTIONAL_LOCKS=0")
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

// gitEnv is the environment without variables that would point git at a
// different repository than the one in the working directory.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// sectionFilter decides, line by line, which sections of a git diff to keep:
// only those for paths that were checked and are not to be dropped. It is an
// allowlist, so a section for a path that appeared after the check, or whose
// header (any "diff --" line) cannot be parsed, is dropped too, to fail
// closed. unparsed counts sections dropped for an unparseable header.
type sectionFilter struct {
	checked, drop map[string]bool
	keep          bool
	started       bool
	unparsed      int
}

func (f *sectionFilter) keepLine(line string) bool {
	if strings.HasPrefix(line, "diff --") {
		f.started = true
		rest, isGit := strings.CutPrefix(line, "diff --git ")
		path, ok := diffHeaderPath(strings.TrimRight(rest, "\n"))
		ok = ok && isGit
		if !ok {
			f.unparsed++
		}
		f.keep = ok && f.checked[path] && !f.drop[path]
	}
	return f.started && f.keep
}

// dropFiles applies a sectionFilter to a whole diff.
func dropFiles(diff string, checked, drop map[string]bool) (string, int) {
	f := &sectionFilter{checked: checked, drop: drop}
	var b strings.Builder
	for _, line := range strings.SplitAfter(diff, "\n") {
		if f.keepLine(line) {
			b.WriteString(line)
		}
	}
	return b.String(), f.unparsed
}

// gitLines runs a git command and passes its output to fn line by line. It
// stops git when fn returns false, or after limit bytes, and reports whether
// output was cut short either way.
func gitLines(ctx context.Context, dir string, limit int, fn func(string) bool, args ...string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), "GIT_OPTIONAL_LOCKS=0")
	var stderr limitedBuffer
	stderr.limit = 64 << 10
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	r := bufio.NewReader(io.LimitReader(stdout, int64(limit)+1))
	read, stopped := 0, false
	var readErr error
	for {
		line, err := r.ReadString('\n')
		read += len(line)
		if read > limit || (line != "" && !fn(line)) {
			stopped = true
			break
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	if stopped {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case stopped:
		return true, nil
	case readErr != nil:
		return false, fmt.Errorf("reading git output: %w", readErr)
	case waitErr != nil:
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return false, fmt.Errorf("git diff: %s", msg)
		}
		return false, fmt.Errorf("git diff: %w", waitErr)
	}
	return false, nil
}

// diffHeaderPath extracts P from "a/P b/P" or the quoted form "\"a/P\" \"b/P\""
// (with --no-renames both sides name the same path).
func diffHeaderPath(rest string) (string, bool) {
	if strings.HasPrefix(rest, `"`) {
		q, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return "", false
		}
		p, err := strconv.Unquote(q)
		if err != nil || !strings.HasPrefix(p, "a/") {
			return "", false
		}
		return p[2:], true
	}
	n := len(rest) - len("a/") - len(" b/")
	if n <= 0 || n%2 != 0 || !strings.HasPrefix(rest, "a/") {
		return "", false
	}
	p := rest[2 : 2+n/2]
	if rest[2+n/2:] != " b/"+p {
		return "", false
	}
	return p, true
}

// filteredPaths returns the paths that have a git filter set in
// .gitattributes. A diff driver alone is not enough: git's built-in ones
// (diff=golang, diff=python, ...) only shape hunk headers, and --no-textconv
// stops any configured textconv from running.
func filteredPaths(ctx context.Context, root string, paths []string) (map[string]bool, error) {
	filtered := map[string]bool{}
	if len(paths) == 0 {
		return filtered, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "check-attr", "-z", "--stdin", "filter")
	cmd.Dir = root
	cmd.Env = append(gitEnv(), "GIT_OPTIONAL_LOCKS=0")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	var out, stderr limitedBuffer
	out.limit, stderr.limit = 16<<20, 64<<10
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git check-attr: %s", strings.TrimSpace(stderr.String()))
	}
	if out.truncated {
		return nil, errors.New("--diff: too many changed files to check their git attributes; use a narrower --base")
	}
	// Records are path, attribute, value, each NUL-terminated.
	f := strings.Split(out.String(), "\x00")
	for i := 0; i+2 < len(f); i += 3 {
		path, attr, value := f[i], f[i+1], f[i+2]
		switch {
		case attr == "filter" && value != "unspecified" && value != "unset":
			filtered[path] = true
		}
	}
	return filtered, nil
}

func gitPaths(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, truncated, err := git(ctx, dir, maxPathsBytes, args...)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("--diff: too many changed files; use a narrower --base")
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
	buf       []byte
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - len(b.buf)
	if len(p) > room {
		b.truncated = true
	}
	if room > 0 {
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
