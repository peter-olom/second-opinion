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
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/peter-olom/second-opinion/internal/harness"
	"github.com/peter-olom/second-opinion/internal/procgroup"
)

// Limits on what --diff attaches. Variables so tests can lower them.
var (
	maxDiffBytes    = 1 << 20    // the git diff itself, after filtered files are dropped
	maxRawDiffBytes = 64 << 20   // how much raw diff to read looking for that
	maxFileBytes    = 256 << 10  // any one attached file
	maxFilesBytes   = 1536 << 10 // all attached files together
	maxPathsBytes   = 1 << 20    // the list of changed and untracked paths
	maxOmitted      = 20         // files listed by name as not attached
	diffTimeout     = 5 * time.Minute
)

// Root returns the top-level directory of the git repository containing dir.
func Root(ctx context.Context, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, _, err := git(ctx, dir, 64<<10, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	// Only git's line endings: a path may legitimately end in whitespace
	// (or a CR).
	root, gitDir, ok := strings.Cut(strings.TrimSuffix(out, "\n"), "\n")
	if !ok {
		return "", fmt.Errorf("git rev-parse: unexpected output %q", out)
	}
	// core.worktree (in a copied .git/config) can name some other folder as
	// the work tree, a parent one included, so repo access or --diff would
	// cover other projects. A real work tree holds the .git that leads to
	// this repository: the folder itself, or a file pointing to it (linked
	// worktrees, submodules).
	if !contains(root, dir) || !ownsGitDir(root, gitDir) {
		return "", fmt.Errorf("git reports %s as the work tree, but it isn't where this repository's .git is (core.worktree?); %w", root, ErrForeignWorkTree)
	}
	return root, nil
}

// ErrForeignWorkTree marks a work tree that doesn't hold its repository's .git.
var ErrForeignWorkTree = errors.New("not using it")

// ownsGitDir reports whether root/.git is gitDir, or a "gitdir:" file that
// points to it.
func ownsGitDir(root, gitDir string) bool {
	want, err := os.Stat(gitDir)
	if err != nil {
		return false
	}
	dotGit := filepath.Join(root, ".git")
	fi, err := os.Lstat(dotGit)
	switch {
	case err != nil:
		return false
	case fi.IsDir():
		return os.SameFile(fi, want)
	case !fi.Mode().IsRegular():
		return false
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	got, err := os.Stat(target)
	return err == nil && os.SameFile(got, want)
}

// contains reports whether dir is root or inside it, by their real paths.
func contains(root, dir string) bool {
	r, err1 := filepath.EvalSymlinks(root)
	d, err2 := filepath.Abs(dir)
	if err2 == nil {
		d, err2 = filepath.EvalSymlinks(d)
	}
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(r, d)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Context is the rendered review context for a set of changes.
type Context struct {
	Text    string
	Summary string // e.g. "diff vs main (merge-base 1a2b3c4), 5 files"
}

// Diff renders the changes in the working tree relative to base. For a branch
// such as "main" the diff starts at the merge-base, so it shows this branch's
// work (committed or not) and none of main's later commits. A non-empty nonce
// is added to the block tags (<file-NONCE …>), so a changed file can't close
// its own block and pass its content off as another file's.
func Diff(ctx context.Context, dir, base, nonce string) (Context, error) {
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()
	if strings.HasPrefix(base, "-") {
		return Context{}, fmt.Errorf("--base %q: a ref cannot start with -", base)
	}
	root, err := Root(ctx, dir)
	if err != nil {
		if ctx.Err() != nil {
			return Context{}, ctx.Err()
		}
		if errors.Is(err, ErrForeignWorkTree) {
			return Context{}, fmt.Errorf("--diff: %w", err)
		}
		return Context{}, errors.New("--diff needs to run inside a git repository")
	}
	// All attachment reads go through an os.Root, which resolves every path
	// component inside the repository: a parent directory swapped for a
	// symlink cannot lead a read outside it.
	repo, err := os.OpenRoot(root)
	if err != nil {
		return Context{}, err
	}
	defer repo.Close()
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
	if base != "HEAD" {
		// Without a merge-base, a diff against the branch tip would show the
		// base's later commits as reverted changes and attach files only it
		// touched, so stop instead.
		mb, _, err := git(ctx, root, 64<<10, "merge-base", base, "HEAD")
		if mb = strings.TrimSpace(mb); err != nil || mb == "" {
			if ctx.Err() != nil {
				return Context{}, ctx.Err()
			}
			return Context{}, fmt.Errorf("--base %s: no common ancestor with HEAD (a shallow clone, or unrelated history); fetch more history or pass a commit", base)
		}
		from = mb
		baseDesc = fmt.Sprintf("%s (merge-base %.7s)", base, mb)
	}

	// Every path in the diff, deletions included, and the untracked files.
	// A truncated list would let files escape the filter check below, so it
	// is an error rather than a partial answer.
	// One listing gives every path and its status, so the attachments, the
	// filter check, and the diff allowlist all agree on the same set.
	// Filter drivers from the git config (clean, process) would run on
	// stat-dirty files while git compares the working tree, and every
	// filtered file is dropped anyway, so they are all switched off.
	// Submodules are not checked for dirt (only their commits count), since
	// git would run their own config's filters for that.
	noFilters, err := filtersOff(ctx, root)
	if err != nil {
		return Context{}, err
	}
	statuses, err := gitPaths(ctx, root, append(noFilters, "diff", "--name-status", "--no-renames", "--ignore-submodules=dirty", "-z", from, "--")...)
	if err != nil {
		return Context{}, err
	}
	if len(statuses)%2 != 0 {
		return Context{}, errors.New("--diff: could not read git's list of changed files")
	}
	var inDiff, changed []string
	listed := map[string]bool{}
	for i := 0; i < len(statuses); i += 2 {
		status, path := statuses[i], statuses[i+1]
		if listed[path] {
			continue // never attach or count a path twice
		}
		listed[path] = true
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
	all := append(inDiff[:len(inDiff):len(inDiff)], untracked...)
	filtered, err := filteredPaths(ctx, root, all, "")
	if err != nil {
		return Context{}, err
	}
	// The check above reads the working tree's .gitattributes, which an
	// uncommitted edit could have stripped of a filter line. So the
	// committed attributes count too: HEAD's and the base's for a branch
	// review (a branch can add a rule that the working tree then drops), and
	// the base's when the change edits a .gitattributes.
	var sources []string
	if base != "HEAD" {
		sources = []string{"HEAD", from}
	} else if slices.ContainsFunc(all, func(p string) bool { return path.Base(p) == ".gitattributes" }) {
		sources = []string{from}
	}
	for _, src := range sources {
		more, err := filteredPaths(ctx, root, all, src)
		if err != nil {
			if ctx.Err() != nil {
				return Context{}, ctx.Err()
			}
			return Context{}, fmt.Errorf("--diff: the committed git attributes could not be checked (git 2.40 or later is needed): %v", err)
		}
		for p := range more {
			filtered[p] = true
		}
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
	// Agent-harness state (conversation history and the like) is dropped
	// from the diff and the attachments like a filtered file.
	harnessState := map[string]bool{}
	drop := map[string]bool{}
	for p := range filtered {
		drop[p] = true
	}
	for _, p := range all {
		if harness.Contains(p) {
			harnessState[p], drop[p] = true, true
		}
	}
	sections := &sectionFilter{checked: checked, drop: drop}
	var kept strings.Builder
	diffTruncated, err := gitLines(ctx, root, maxRawDiffBytes, func(line string) bool {
		if sections.keepLine(line) {
			if kept.Len()+len(line) > maxDiffBytes {
				return false
			}
			kept.WriteString(line)
		}
		return true
	}, append(noFilters, "-c", "core.quotePath=false", "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false",
		"diff", "--src-prefix=a/", "--dst-prefix=b/", "--submodule=short",
		"--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=dirty", from, "--")...)
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
	diffTag, fileTag := Tag("git-diff", nonce), Tag("file", nonce)
	fmt.Fprintf(&b, "<%s base=%q>\n%s\n</%s>", diffTag, baseDesc, strings.TrimRight(diff, "\n"), diffTag)

	var omitted, attached []string
	total := 0
	// Filtered files that were deleted never reach attach; note them too.
	isChanged := map[string]bool{}
	for _, p := range changed {
		isChanged[p] = true
	}
	for _, p := range inDiff {
		switch {
		case isChanged[p]:
		case harnessState[p]:
			omitted = append(omitted, showPath(p)+" (deleted; agent-harness state)")
		case filtered[p]:
			omitted = append(omitted, showPath(p)+" (deleted; git filter, may be encrypted)")
		}
	}
	attach := func(path, status string) {
		if harnessState[path] {
			omitted = append(omitted, showPath(path)+" (agent-harness state; not attached)")
			return
		}
		if filtered[path] {
			omitted = append(omitted, showPath(path)+" (git filter; may be encrypted)")
			return
		}
		if total >= maxFilesBytes {
			omitted = append(omitted, showPath(path)+" (attachment budget exceeded)")
			return
		}
		data, why := readAttachment(repo, path, min(maxFileBytes, maxFilesBytes-total))
		if why != "" {
			omitted = append(omitted, showPath(path)+" ("+why+")")
			return
		}
		total += len(data)
		attached = append(attached, path)
		fmt.Fprintf(&b, "\n\n<%s path=%q status=%q>\n%s\n</%s>", fileTag, path, status, strings.TrimRight(string(data), "\n"), fileTag)
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

	// Name what was sent, so the caller can see it.
	summary := fmt.Sprintf("diff vs %s", baseDesc)
	if len(attached) > 0 {
		var names []string
		for _, p := range attached[:min(len(attached), 10)] {
			names = append(names, showPath(p))
		}
		summary += fmt.Sprintf("; %d file%s attached: %s", len(attached), plural(len(attached)), strings.Join(names, ", "))
		if rest := len(attached) - len(names); rest > 0 {
			summary += fmt.Sprintf(", and %d more", rest)
		}
	}
	if len(omitted) > 0 {
		summary += fmt.Sprintf("; %d not attached", len(omitted))
	}
	return Context{Text: b.String(), Summary: summary}, nil
}

// readAttachment reads a file (a path relative to repo) for attaching, or
// explains why it is not attached. Only regular files are read, without
// following symlinks, and never more than limit bytes.
func readAttachment(repo *os.Root, path string, limit int) ([]byte, string) {
	fi, err := repo.Lstat(path)
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
	f, err := repo.Open(path)
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
// Output goes through a writer, so os/exec owns the copy and WaitDelay can
// close the pipe even if a helper git started holds it. fsmonitor hooks are
// disabled, and git is told not to take optional locks.
func git(ctx context.Context, dir string, limit int, args ...string) (string, bool, error) {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	out := &limitedBuffer{limit: limit, full: stop}
	var stderr limitedBuffer
	stderr.limit = 64 << 10
	err := runGit(runCtx, dir, out, &stderr, args...)
	switch {
	case ctx.Err() != nil:
		return "", false, ctx.Err()
	case out.truncated:
		return string(out.buf), true, nil
	case err != nil:
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", false, fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", false, fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out.buf), false, nil
}

// runGit runs git with the shared safety settings and the given output writers.
func runGit(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), "GIT_OPTIONAL_LOCKS=0")
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Git can start filter helpers from the user's git config; in a process
	// group, they die with git on cancellation and can't outlive it.
	procgroup.Isolate(cmd)
	err := cmd.Run()
	procgroup.Kill(cmd)
	// If git exited successfully but a helper it started kept the output
	// pipe open, WaitDelay closed it; git's own output is complete.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return nil
	}
	return err
}

// gitEnv is the environment without variables that would point git at a
// different repository than the one in the working directory.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_PREFIX",
			// GIT_CONFIG redirects only `git config`, so filter discovery
			// would miss drivers that `git diff` still runs.
			"GIT_CONFIG":
			continue
		}
		env = append(env, kv)
	}
	// Never prompt for credentials (git runs in its own process group, so a
	// prompt would only stall), and never fetch missing objects in a
	// partial clone: collecting a diff stays local.
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
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
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	lw := &lineWriter{fn: fn, limit: limit, stop: stop}
	var stderr limitedBuffer
	stderr.limit = 64 << 10
	err := runGit(runCtx, dir, lw, &stderr, args...)
	lw.flush()
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case lw.stopped:
		return true, nil
	case err != nil:
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return false, fmt.Errorf("git diff: %s", msg)
		}
		return false, fmt.Errorf("git diff: %w", err)
	}
	return false, nil
}

// lineWriter splits what is written to it into lines for fn, and stops (by
// cancelling the command) when fn returns false or limit bytes have arrived.
type lineWriter struct {
	fn      func(string) bool
	limit   int
	stop    func()
	read    int
	partial []byte
	stopped bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.stopped {
		return n, nil
	}
	w.read += n
	if w.read > w.limit {
		w.halt()
		return n, nil
	}
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := string(w.partial[:i+1])
		w.partial = w.partial[i+1:]
		if !w.fn(line) {
			w.halt()
			return n, nil
		}
	}
	return n, nil
}

// flush hands a final line without a newline to fn.
func (w *lineWriter) flush() {
	if !w.stopped && len(w.partial) > 0 {
		if !w.fn(string(w.partial)) {
			w.stopped = true
		}
		w.partial = nil
	}
}

func (w *lineWriter) halt() {
	w.stopped = true
	w.partial = nil
	w.stop()
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
func filteredPaths(ctx context.Context, root string, paths []string, source string) (map[string]bool, error) {
	filtered := map[string]bool{}
	if len(paths) == 0 {
		return filtered, nil
	}
	var out, stderr limitedBuffer
	out.limit, stderr.limit = 16<<20, 64<<10
	args := []string{"-c", "core.fsmonitor=false", "check-attr", "-z", "--stdin"}
	if source != "" {
		args = append(args, "--source", source) // that commit's .gitattributes
	}
	cmd := exec.CommandContext(ctx, "git", append(args, "filter")...)
	cmd.Dir = root
	cmd.Env = append(gitEnv(), "GIT_OPTIONAL_LOCKS=0")
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	cmd.Stdout, cmd.Stderr = &out, &stderr
	procgroup.Isolate(cmd)
	err := cmd.Run()
	procgroup.Kill(cmd)
	if err != nil {
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

// limitedBuffer keeps the first limit bytes written to it. If full is set,
// it is called once the limit is exceeded, to stop the writer early.
type limitedBuffer struct {
	buf       []byte
	limit     int
	truncated bool
	full      func()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - len(b.buf)
	if len(p) > room {
		if !b.truncated && b.full != nil {
			b.full()
		}
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

// Tag names an attachment block: name, or name-nonce with a nonce.
func Tag(name, nonce string) string {
	if nonce == "" {
		return name
	}
	return name + "-" + nonce
}

// showPath prints a file name as is when it is plain text, and quoted (Go
// syntax) when it holds anything else. File names come from the repository,
// which may be untrusted, and a name with a newline or escape sequence could
// otherwise fake prompt structure or so's output, or rewrite the terminal.
func showPath(p string) string {
	for _, r := range p {
		if r == '"' || r == '\\' || !unicode.IsGraphic(r) && r != ' ' {
			return strconv.Quote(p)
		}
	}
	return p
}

// filtersOff returns git -c options that switch off every filter driver in
// the git config for this repository: empty clean, smudge and process
// commands are no-ops, and required = false keeps git from failing for want
// of one.
func filtersOff(ctx context.Context, root string) ([]string, error) {
	out, truncated, err := git(ctx, root, 1<<20, "config", "--name-only", "--get-regexp", `^filter\.`)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil // no filter drivers configured
		}
		return nil, err
	}
	if truncated {
		return nil, errors.New("--diff: too many git filter settings to switch them all off")
	}
	seen := map[string]bool{}
	var args []string
	for _, key := range strings.Split(strings.TrimSpace(out), "\n") {
		i := strings.LastIndexByte(key, '.')
		// i == len("filter.") is a driver with an empty name (filter..clean).
		if !strings.HasPrefix(key, "filter.") || i < len("filter.") {
			continue
		}
		name := key[len("filter."):i]
		if seen[name] {
			continue
		}
		seen[name] = true
		if strings.Contains(name, "=") {
			return nil, fmt.Errorf("--diff: can't switch off the git filter driver %q (its name has an =)", name)
		}
		for _, k := range []string{"clean", "smudge", "process"} {
			args = append(args, "-c", "filter."+name+"."+k+"=")
		}
		args = append(args, "-c", "filter."+name+".required=false")
	}
	return args, nil
}

// HarnessPaths lists harness state anywhere in the repository at root,
// tracked, untracked, or ignored, up to max of them.
func HarnessPaths(ctx context.Context, root string, max int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Tracked and untracked files in full, then ignored ones collapsed to
	// their folders (node_modules/ and the like stay one entry each).
	paths, err := gitPaths(ctx, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	ignored, err := gitPaths(ctx, root, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, err
	}
	paths = append(paths, ignored...)
	var found []string
	seen := map[string]bool{}
	for _, p := range paths {
		if !harness.Contains(strings.TrimSuffix(p, "/")) {
			continue
		}
		// Report the harness folder itself, not every file in it.
		parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
		for i, part := range parts {
			if harness.Contains(part) {
				p = strings.Join(parts[:i+1], "/")
				break
			}
		}
		if !seen[p] {
			seen[p] = true
			found = append(found, p)
			if len(found) >= max {
				break
			}
		}
	}
	return found, nil
}
