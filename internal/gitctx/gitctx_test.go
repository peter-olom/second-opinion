package gitctx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo creates a repo with a commit on main and a feature branch checked out.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 1 }\n")
	write(t, dir, "gone.go", "package a\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "init")
	run(t, dir, "checkout", "-q", "-b", "feature")
	return dir
}

func TestDiffUncommitted(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 2 }\n")
	write(t, dir, "new.go", "package a\n\nfunc New() {}\n")
	write(t, dir, "blob.bin", "\x00\x01\x02")
	os.Remove(filepath.Join(dir, "gone.go"))

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<git-diff base="HEAD">`,
		"+func A() int { return 2 }",
		"<file path=\"a.go\" status=\"changed\">\npackage a\n\nfunc A() int { return 2 }\n</file>",
		"<file path=\"new.go\" status=\"new, untracked\">\npackage a\n\nfunc New() {}\n</file>",
		"blob.bin (binary)",
	} {
		if !strings.Contains(ctx.Text, want) {
			t.Errorf("context missing %q:\n%s", want, ctx.Text)
		}
	}
	if strings.Contains(ctx.Text, `<file path="gone.go"`) {
		t.Error("deleted files should appear in the diff only, not as attachments")
	}
}

func TestDiffAgainstBranchUsesMergeBase(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "feature.go", "package a\n\nfunc F() {}\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "feature work")
	// A later commit on main must not show up in the feature branch's diff.
	run(t, dir, "checkout", "-q", "main")
	write(t, dir, "mainonly.go", "package a\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "main work")
	run(t, dir, "checkout", "-q", "feature")

	ctx, err := Diff(context.Background(), dir, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx.Text, `<file path="feature.go" status="changed">`) {
		t.Errorf("committed branch work missing:\n%s", ctx.Text)
	}
	if strings.Contains(ctx.Text, "mainonly.go") {
		t.Errorf("main's later commits should be excluded:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Summary, "merge-base") {
		t.Errorf("summary = %q, want merge-base mentioned", ctx.Summary)
	}
}

func TestDiffErrors(t *testing.T) {
	dir := newRepo(t)
	if _, err := Diff(context.Background(), dir, "HEAD", ""); err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Errorf("clean tree: err = %v, want no changes", err)
	}
	if _, err := Diff(context.Background(), dir, "nope", ""); err == nil || !strings.Contains(err.Error(), "unknown base") {
		t.Errorf("bad base: err = %v", err)
	}
	if _, err := Diff(context.Background(), t.TempDir(), "HEAD", ""); err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Errorf("outside repo: err = %v", err)
	}
}

func TestDiffNeverFollowsSymlinks(t *testing.T) {
	dir := newRepo(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	write(t, filepath.Dir(outside), "secret.txt", "OUTSIDE-SECRET")
	// An untracked symlink to a file outside the repo, and one to an ignored .env inside it.
	write(t, dir, ".gitignore", ".env\n")
	write(t, dir, ".env", "INSIDE-SECRET")
	if err := os.Symlink(outside, filepath.Join(dir, "innocent.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	os.Symlink(filepath.Join(dir, ".env"), filepath.Join(dir, "config.txt"))

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"OUTSIDE-SECRET", "INSIDE-SECRET"} {
		if strings.Contains(ctx.Text, secret) {
			t.Errorf("symlink target %s was attached:\n%s", secret, ctx.Text)
		}
	}
	if !strings.Contains(ctx.Text, "innocent.txt (symlink, not followed)") {
		t.Errorf("skipped symlink should be listed:\n%s", ctx.Text)
	}
}

func TestDiffSkipsFIFOWithoutHanging(t *testing.T) {
	// Git never lists an untracked FIFO, but it does report a tracked file
	// that has been replaced by one. Reading it would block forever.
	dir := newRepo(t)
	os.Remove(filepath.Join(dir, "a.go"))
	if err := mkfifo(filepath.Join(dir, "a.go")); err != nil {
		t.Skip("FIFOs unavailable:", err)
	}
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		ctx, err := Diff(context.Background(), dir, "HEAD", "")
		done <- result{ctx.Text, err}
	}()
	select {
	case r := <-done:
		// Either git refuses the FIFO outright ("unsupported file type") or
		// it is listed as not attached; both are safe.
		switch {
		case r.err != nil && !strings.Contains(r.err.Error(), "unsupported file type"):
			t.Errorf("err = %v, want git's refusal of the FIFO", r.err)
		case r.err == nil && !strings.Contains(r.text, "a.go (not a regular file)"):
			t.Errorf("FIFO should be listed as not attached:\n%s", r.text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Diff hung on a FIFO")
	}
}

func TestDiffBoundsSizes(t *testing.T) {
	defer func(f, total, omitted int) { maxFileBytes, maxFilesBytes, maxOmitted = f, total, omitted }(maxFileBytes, maxFilesBytes, maxOmitted)
	maxFileBytes, maxFilesBytes, maxOmitted = 100, 250, 3
	dir := newRepo(t)
	write(t, dir, "big.txt", strings.Repeat("x", 101))
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt", "f.txt"} {
		write(t, dir, name, strings.Repeat("y", 90))
	}

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx.Text, "big.txt (too large)") {
		t.Errorf("oversized file should be skipped:\n%s", ctx.Text)
	}
	if n := strings.Count(ctx.Text, "<file path="); n != 2 {
		t.Errorf("attached %d files, want 2 within the 250-byte budget", n)
	}
	if !strings.Contains(ctx.Text, "and 2 more") {
		t.Errorf("omitted list should be capped at 3 names:\n%s", ctx.Text)
	}
}

func TestDiffTruncatesLongDiff(t *testing.T) {
	defer func(d int) { maxDiffBytes = d }(maxDiffBytes)
	maxDiffBytes = 200
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n\n"+strings.Repeat("// long line of changes\n", 100))
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	diff := ctx.Text[:strings.Index(ctx.Text, "</git-diff>")]
	if !strings.Contains(diff, "[diff truncated]") || len(diff) > 400 {
		t.Errorf("diff should be cut at the limit (%d bytes):\n%s", len(diff), diff)
	}
}

func TestDiffDoesNotRunTextconv(t *testing.T) {
	dir := newRepo(t)
	marker := filepath.Join(t.TempDir(), "textconv-ran")
	run(t, dir, "config", "diff.leak.textconv", "sh -c 'touch "+marker+"; echo DECRYPTED-SECRET'")
	write(t, dir, ".gitattributes", "*.enc diff=leak\n")
	write(t, dir, "s.enc", "ciphertext-1\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "enc")
	write(t, dir, "s.enc", "ciphertext-2\n")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Text, "DECRYPTED-SECRET") {
		t.Errorf("textconv output was attached:\n%s", ctx.Text)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the textconv driver ran")
	}
	gitRunsDriver(t, dir, marker, "diff", "HEAD")
}

func TestDiffCancels(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Diff(ctx, dir, "HEAD", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestDiffCancelsWhileGitRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell wrapper for git")
	}
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// A git that stalls in the diff itself, after signalling it got there.
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	wrapper := "#!/bin/sh\ncase \"$*\" in *--src-prefix=a/*) touch " + started + "; sleep 10;; esac\nexec " + realGit + " \"$@\"\n"
	os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	start := time.Now()
	_, err = Diff(ctx, dir, "HEAD", "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s; cancellation should stop git", d)
	}
	if _, err := os.Stat(started); err != nil {
		t.Error("the diff never ran, so this test checked nothing")
	}
}

func TestFilterDriversNeverRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell filter")
	}
	dir := newRepo(t)
	write(t, dir, ".gitattributes", "*.x filter=evil\n*.y filter=other\n*.z filter=\n")
	write(t, dir, "a.x", "one\n")
	write(t, dir, "b.y", "one\n")
	write(t, dir, "c.z", "one\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "x")
	marker := filepath.Join(t.TempDir(), "ran")
	// Configured after the commit, so only Diff could run them.
	run(t, dir, "config", "filter.evil.clean", "sh -c 'touch "+marker+".clean; cat'")
	run(t, dir, "config", "filter.evil.required", "true")
	run(t, dir, "config", "filter.other.process", "sh -c 'touch "+marker+".process; exit 1'")
	// A driver with an empty name, which "*.z filter=" selects.
	run(t, dir, "config", "filter..clean", "sh -c 'touch "+marker+".empty; cat'")
	write(t, dir, "a.x", "two\n")
	write(t, dir, "b.y", "two\n")
	write(t, dir, "c.z", "two\n")
	if _, err := Diff(context.Background(), dir, "HEAD", ""); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{marker + ".clean", marker + ".process", marker + ".empty"} {
		if _, err := os.Stat(m); err == nil {
			t.Errorf("a filter driver from the git config ran during --diff (%s)", filepath.Base(m))
		}
	}
	// (Plain git never selects the empty-named driver through "filter=",
	// so it has no control; so switches it off regardless.)
	for _, m := range []string{marker + ".clean", marker + ".process"} {
		// New times, so git can't trust its cached stat data and compares again.
		later := time.Now().Add(time.Minute)
		for _, f := range []string{"a.x", "b.y", "c.z"} {
			os.Chtimes(filepath.Join(dir, f), later, later)
		}
		gitRunsDriver(t, dir, m, "diff", "HEAD")
	}
}

func TestDiffSkipsGitCryptPlaintext(t *testing.T) {
	// git-crypt keeps plaintext in the working tree and ciphertext in the
	// repository. Neither the diff nor the attachments may carry the plaintext.
	dir := newRepo(t)
	run(t, dir, "config", "filter.git-crypt.clean", "cat")
	run(t, dir, "config", "filter.git-crypt.smudge", "cat")
	write(t, dir, ".gitattributes", "secrets.env filter=git-crypt diff=git-crypt\n")
	write(t, dir, "secrets.env", "API_KEY=old\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "secrets")
	write(t, dir, "secrets.env", "API_KEY=PLAINTEXT-SECRET\n")
	write(t, dir, "new.env", "TOKEN=NEW-PLAINTEXT\n")
	write(t, dir, ".gitattributes", "*.env filter=git-crypt diff=git-crypt\n")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PLAINTEXT-SECRET", "NEW-PLAINTEXT"} {
		if strings.Contains(ctx.Text, secret) {
			t.Errorf("filtered file content %s was sent:\n%s", secret, ctx.Text)
		}
	}
	if !strings.Contains(ctx.Text, "secrets.env (git filter; may be encrypted)") {
		t.Errorf("filtered file should be listed:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Text, "return 3") {
		t.Errorf("ordinary changes should still be sent:\n%s", ctx.Text)
	}
}

func TestDiffIgnoresGitDirFromEnvironment(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")
	other := newRepo(t)
	write(t, other, "a.go", "package a\n\nfunc A() int { return 99 }\n")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Text, "return 99") || !strings.Contains(ctx.Text, "return 3") {
		t.Errorf("collected from GIT_DIR instead of the working directory:\n%s", ctx.Text)
	}
}

func TestDiffSkipsFilterOnlyFiles(t *testing.T) {
	// A filter with no diff driver (a sops-style clean/smudge pair) still
	// means the working copy may be plaintext.
	dir := newRepo(t)
	run(t, dir, "config", "filter.sops.clean", "cat")
	run(t, dir, "config", "filter.sops.smudge", "cat")
	write(t, dir, ".gitattributes", "vault.yaml filter=sops\n")
	write(t, dir, "vault.yaml", "password: PLAINTEXT-VAULT\n")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Text, "PLAINTEXT-VAULT") {
		t.Errorf("filter-only file was sent:\n%s", ctx.Text)
	}
}

func TestDiffKeepsFilesWithBuiltinDiffDrivers(t *testing.T) {
	// diff=golang only shapes hunk headers; such files must be sent normally.
	dir := newRepo(t)
	write(t, dir, ".gitattributes", "*.go diff=golang\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "attrs")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 42 }\n")

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	diff := ctx.Text[:strings.Index(ctx.Text, "</git-diff>")]
	if !strings.Contains(diff, "return 42") || !strings.Contains(ctx.Text, `<file path="a.go"`) {
		t.Errorf("a .go file with diff=golang was dropped:\n%s", ctx.Text)
	}
}

func TestDiffDropsDeletedFilteredFile(t *testing.T) {
	dir := newRepo(t)
	run(t, dir, "config", "filter.crypt.clean", "cat")
	run(t, dir, "config", "filter.crypt.smudge", "cat")
	write(t, dir, ".gitattributes", "*.secret filter=crypt\n")
	write(t, dir, "db.secret", "PASSWORD=DELETED-SECRET\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "secret")
	os.Remove(filepath.Join(dir, "db.secret"))
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Text, "DELETED-SECRET") {
		t.Errorf("a deleted filtered file appeared in the diff:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Text, "return 3") {
		t.Errorf("ordinary changes should still be sent:\n%s", ctx.Text)
	}
}

func TestDiffManyFilteredFiles(t *testing.T) {
	// One exclusion per file used to go on the command line; many LFS-style
	// files must not break --diff.
	dir := newRepo(t)
	run(t, dir, "config", "filter.lfs.clean", "cat")
	run(t, dir, "config", "filter.lfs.smudge", "cat")
	write(t, dir, ".gitattributes", "*.bin filter=lfs\n")
	for i := range 3000 {
		write(t, dir, fmt.Sprintf("asset-%04d-with-a-long-name-to-fill-argv.bin", i), "data\n")
	}
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx.Text, "return 3") {
		t.Errorf("ordinary changes should still be sent")
	}
}

func TestDropFiles(t *testing.T) {
	diff := "diff --git a/keep.go b/keep.go\n+keep\n" +
		"diff --git a/drop me.txt b/drop me.txt\n+DROPPED\n" +
		"diff --git \"a/we\\\"ird.txt\" \"b/we\\\"ird.txt\"\n+QUOTED-DROPPED\n" +
		"diff --git garbage header\n+UNPARSEABLE\n" +
		"diff --git a/also-keep.go b/also-keep.go\n+also\n" +
		"diff --git \"a/keep \\\"quoted\\\".go\" \"b/keep \\\"quoted\\\".go\"\n+QUOTED-KEPT\n"
	checked := map[string]bool{"keep.go": true, "drop me.txt": true, `we"ird.txt`: true, "also-keep.go": true, `keep "quoted".go`: true}
	got, unparsed := dropFiles(diff, checked, map[string]bool{"drop me.txt": true, `we"ird.txt`: true})
	if unparsed != 1 {
		t.Errorf("unparsed = %d, want 1 (the garbage header)", unparsed)
	}
	for _, gone := range []string{"DROPPED", "QUOTED-DROPPED", "UNPARSEABLE"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s should have been dropped:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"+keep", "+also", "+QUOTED-KEPT"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s should have been kept:\n%s", kept, got)
		}
	}
}

func TestDiffFailsClosedOnTooManyPaths(t *testing.T) {
	defer func(n int) { maxPathsBytes = n }(maxPathsBytes)
	maxPathsBytes = 50
	dir := newRepo(t)
	for i := range 20 {
		write(t, dir, fmt.Sprintf("file-%02d.txt", i), "x\n")
	}
	if _, err := Diff(context.Background(), dir, "HEAD", ""); err == nil || !strings.Contains(err.Error(), "too many changed files") {
		t.Errorf("err = %v, want a refusal rather than a partial check", err)
	}
}

// filteredRepo has a git filter on *.secret, one changed secret and one changed ordinary file.
func filteredRepo(t *testing.T) string {
	t.Helper()
	dir := newRepo(t)
	run(t, dir, "config", "filter.crypt.clean", "cat")
	run(t, dir, "config", "filter.crypt.smudge", "cat")
	write(t, dir, ".gitattributes", "*.secret filter=crypt\n")
	write(t, dir, "db.secret", "PASSWORD=old\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "secret")
	write(t, dir, "db.secret", "PASSWORD=FILTERED-SECRET\n")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")
	return dir
}

func TestDiffSurvivesCustomPrefixes(t *testing.T) {
	for _, cfg := range [][2]string{{"diff.mnemonicPrefix", "true"}, {"diff.noprefix", "true"}} {
		for _, withUntracked := range []bool{false, true} {
			dir := filteredRepo(t)
			run(t, dir, "config", cfg[0], cfg[1])
			if withUntracked {
				write(t, dir, "new.txt", "new\n")
			}
			ctx, err := Diff(context.Background(), dir, "HEAD", "")
			if err != nil {
				t.Fatalf("%s: %v", cfg[0], err)
			}
			diff := ctx.Text[:strings.Index(ctx.Text, "</git-diff>")]
			if !strings.Contains(diff, "return 3") {
				t.Errorf("%s (untracked=%v): the ordinary change was lost:\n%s", cfg[0], withUntracked, ctx.Text)
			}
			if strings.Contains(ctx.Text, "FILTERED-SECRET") {
				t.Errorf("%s: filtered content was sent", cfg[0])
			}
		}
	}
}

func TestDiffWhenOnlyFilteredFilesChanged(t *testing.T) {
	dir := filteredRepo(t)
	run(t, dir, "checkout", "--", "a.go") // only the filtered file is changed now
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatalf("a change to a filtered file is still a change, not %v", err)
	}
	if !strings.Contains(ctx.Text, "db.secret (git filter; may be encrypted)") || strings.Contains(ctx.Text, "FILTERED-SECRET") {
		t.Errorf("filtered file should be listed, not sent:\n%s", ctx.Text)
	}
}

func TestDiffBudgetIgnoresFilteredContent(t *testing.T) {
	defer func(d int) { maxDiffBytes = d }(maxDiffBytes)
	maxDiffBytes = 300
	dir := filteredRepo(t)
	// A filtered file that sorts first and is bigger than the diff limit.
	write(t, dir, "0big.secret", strings.Repeat("SECRET LINE\n", 100))
	run(t, dir, "add", "0big.secret")
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if diff := ctx.Text[:strings.Index(ctx.Text, "</git-diff>")]; !strings.Contains(diff, "return 3") {
		t.Errorf("a large filtered file used up the diff budget for ordinary changes:\n%s", diff)
	}
}

func TestDiffNotesFilteredDeletions(t *testing.T) {
	dir := filteredRepo(t)
	os.Remove(filepath.Join(dir, "db.secret"))
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx.Text, "db.secret (deleted; git filter, may be encrypted)") {
		t.Errorf("filtered deletion should be noted:\n%s", ctx.Text)
	}
}

func TestDropFilesAllowlistAndBoundaries(t *testing.T) {
	// Each suspect section follows a kept one, so it is only dropped if it is
	// recognised as a section boundary of its own.
	diff := "diff --git a/checked.go b/checked.go\n+ok\n" +
		"diff --cc conflicted.secret\n+COMBINED\n" +
		"diff --git a/checked2.go b/checked2.go\n+ok2\n" +
		"diff --git a/late.secret b/late.secret\n+APPEARED-AFTER-CHECK\n"
	got, _ := dropFiles(diff, map[string]bool{"checked.go": true, "checked2.go": true}, nil)
	for _, gone := range []string{"APPEARED-AFTER-CHECK", "COMBINED"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s should have been dropped:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "+ok\n") || !strings.Contains(got, "+ok2") {
		t.Errorf("checked sections should be kept:\n%s", got)
	}
}

func TestDiffFilteredFileChangedMidCollection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell wrapper for git")
	}
	dir := filteredRepo(t)
	run(t, dir, "checkout", "--", "db.secret") // the filtered file is clean when listed
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// A git wrapper that logs every call and, just before the diff itself
	// runs, writes plaintext into the filtered file: the race the review found.
	bin := t.TempDir()
	logFile := filepath.Join(bin, "calls")
	wrapper := "#!/bin/sh\necho \"$*\" >> " + logFile + "\n" +
		"case \"$*\" in *--src-prefix=a/*) echo 'PASSWORD=RACED-PLAINTEXT' > " + filepath.Join(dir, "db.secret") + ";; esac\n" +
		"exec " + realGit + " \"$@\"\n"
	os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Text, "RACED-PLAINTEXT") {
		t.Errorf("a filtered file changed after the check was sent:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Text, "return 3") {
		t.Errorf("the ordinary change should still be sent:\n%s", ctx.Text)
	}
	calls, _ := os.ReadFile(logFile)
	n := 0
	for _, l := range strings.Split(string(calls), "\n") {
		if strings.Contains(l, " diff ") && (strings.Contains(l, "--name-status") || strings.Contains(l, "--name-only")) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("listed changed files %d times, want once:\n%s", n, calls)
	}
}

func TestDiffSubmodulesUseShortFormat(t *testing.T) {
	sub := newRepo(t)
	marker := filepath.Join(t.TempDir(), "textconv-ran")
	write(t, sub, ".gitattributes", "*.enc diff=leak\n")
	write(t, sub, "s.enc", "v1\n")
	run(t, sub, "add", ".")
	run(t, sub, "commit", "-q", "-m", "enc")

	dir := newRepo(t)
	run(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	run(t, dir, "commit", "-q", "-m", "add sub")
	subWT := filepath.Join(dir, "sub")
	run(t, subWT, "config", "diff.leak.textconv", "sh -c 'touch "+marker+"; cat'")
	write(t, subWT, "s.enc", "v2\n") // a dirty submodule
	run(t, subWT, "commit", "-q", "-am", "bump")
	write(t, subWT, "s.enc", "v3\n")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")

	for _, mode := range []string{"log", "diff"} {
		run(t, dir, "config", "diff.submodule", mode)
		ctx, err := Diff(context.Background(), dir, "HEAD", "")
		if err != nil {
			t.Fatalf("diff.submodule=%s: %v", mode, err)
		}
		diff := ctx.Text[:strings.Index(ctx.Text, "</git-diff>")]
		if !strings.Contains(diff, "diff --git a/sub b/sub") || !strings.Contains(diff, "Subproject commit") {
			t.Errorf("diff.submodule=%s: want a short submodule section:\n%s", mode, diff)
		}
		if strings.Contains(diff, "Submodule sub") || strings.Contains(diff, "a/sub/s.enc") {
			t.Errorf("diff.submodule=%s: config leaked into the format:\n%s", mode, diff)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a textconv driver inside the submodule ran")
	}
}

func TestDiffDuringMergeConflict(t *testing.T) {
	dir := newRepo(t)
	run(t, dir, "checkout", "-q", "-b", "other")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 10 }\n")
	run(t, dir, "commit", "-q", "-am", "other")
	run(t, dir, "checkout", "-q", "feature")
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 20 }\n")
	run(t, dir, "commit", "-q", "-am", "feature")
	cmd := exec.Command("git", "merge", "-q", "other")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	cmd.Run() // expected to stop with a conflict

	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(ctx.Text, `<file path="a.go"`); n != 1 {
		t.Errorf("conflicted a.go attached %d times, want once:\n%s", n, ctx.Text)
	}
}

func TestReadAttachmentStaysInsideTheRepo(t *testing.T) {
	// A parent directory that is (or was swapped for) a symlink to somewhere
	// outside the repository must not lead a read out of it.
	dir := t.TempDir()
	outside := t.TempDir()
	write(t, outside, "secret.txt", "OUTSIDE-SECRET")
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	repo, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	data, why := readAttachment(repo, "sub/secret.txt", 1000)
	if why == "" || strings.Contains(string(data), "OUTSIDE-SECRET") {
		t.Errorf("read through a symlinked parent: %q (%q)", data, why)
	}
}

func TestDiffSummaryNamesAttachedFiles(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")
	write(t, dir, "notes.md", "hello\n")
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.go", "notes.md"} {
		if !strings.Contains(ctx.Summary, name) {
			t.Errorf("summary %q should name %s", ctx.Summary, name)
		}
	}
}

func TestRootKeepsTrailingWhitespace(t *testing.T) {
	parent := t.TempDir()
	spaced := filepath.Join(parent, "app ")
	sibling := filepath.Join(parent, "app")
	for _, d := range []string{spaced, sibling} {
		os.MkdirAll(d, 0o755)
		if out, err := exec.Command("git", "-C", d, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}
	got, err := Root(context.Background(), spaced)
	want, _ := filepath.EvalSymlinks(spaced)
	if err != nil || got != want {
		t.Errorf("Root = %q, %v; want %q (not the sibling without the space)", got, err, want)
	}
}

func TestDiffRejectsOptionLikeBase(t *testing.T) {
	dir := newRepo(t)
	if _, err := Diff(context.Background(), dir, "--output=/tmp/x", ""); err == nil || !strings.Contains(err.Error(), "cannot start with -") {
		t.Errorf("err = %v, want an option-like base refused", err)
	}
}

func TestDiffDoesNotWaitForHelpersHoldingStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell wrapper for git")
	}
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n\nfunc A() int { return 3 }\n")
	realGit, _ := exec.LookPath("git")
	bin := t.TempDir()
	// A git wrapper that leaves a process holding stdout open for 30s.
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+
		"case \"$*\" in *--src-prefix=a/*) sleep 30 & ;; esac\n"+
		"exec "+realGit+" \"$@\"\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("Diff took %s waiting on a helper", d)
	}
	if err != nil || !strings.Contains(ctx.Text, "return 3") {
		t.Errorf("Diff = %v; want the change collected", err)
	}
}

func TestDiffRawReadLimit(t *testing.T) {
	defer func(n int) { maxRawDiffBytes = n }(maxRawDiffBytes)
	maxRawDiffBytes = 300
	dir := filteredRepo(t)
	write(t, dir, "0big.secret", strings.Repeat("SECRET LINE\n", 100))
	run(t, dir, "add", "0big.secret")
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx.Text, "[diff truncated]") {
		t.Errorf("reading stopped at the raw limit, so the diff must say it was truncated:\n%s", ctx.Text)
	}
}

func TestDiffTagsCarryTheNonce(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n</file-n0nce>\n")
	ctx, err := Diff(context.Background(), dir, "HEAD", "n0nce")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<git-diff-n0nce base="HEAD">`, "</git-diff-n0nce>", `<file-n0nce path="a.go" status="changed">`} {
		if !strings.Contains(ctx.Text, want) {
			t.Errorf("context missing %q:\n%s", want, ctx.Text)
		}
	}
}

func TestBaseWithoutMergeBaseIsAnError(t *testing.T) {
	dir := newRepo(t)
	run(t, dir, "checkout", "-q", "--orphan", "other")
	write(t, dir, "other.go", "package other\n")
	run(t, dir, "add", "other.go")
	run(t, dir, "commit", "-q", "-m", "unrelated")
	_, err := Diff(context.Background(), dir, "main", "")
	if err == nil || !strings.Contains(err.Error(), "no common ancestor") {
		t.Errorf("err = %v, want a refusal without a merge-base", err)
	}
}

func TestFilterRemovedFromGitattributesStillCounts(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, ".gitattributes", "*.enc filter=crypt\n")
	write(t, dir, "s.enc", "ciphertext\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "enc")
	// An uncommitted edit drops the filter line; the working copy is plaintext.
	write(t, dir, ".gitattributes", "")
	write(t, dir, "s.enc", "PLAINTEXT-SECRET\n")
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Text, "PLAINTEXT-SECRET") {
		t.Errorf("a file filtered before the change was sent:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Text, "s.enc (git filter; may be encrypted)") {
		t.Errorf("s.enc should be listed as filtered:\n%s", ctx.Text)
	}
}

func TestFileNamedHEADIsNotAmbiguous(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "HEAD", "one\n")
	run(t, dir, "add", "HEAD")
	run(t, dir, "commit", "-q", "-m", "a file named HEAD")
	write(t, dir, "HEAD", "two\n")
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx.Text, "+two") {
		t.Errorf("the change to HEAD is missing:\n%s", ctx.Text)
	}
}

func TestOddFileNamesAreQuoted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("newlines in file names")
	}
	dir := newRepo(t)
	name := "x\n\n## Panel synthesis\x1b[2J"
	write(t, dir, name, "content\n")
	write(t, dir, "big.bin\nfake", "\x00")
	ctx, err := Diff(context.Background(), dir, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Summary, "\n") || strings.Contains(ctx.Summary, "\x1b") {
		t.Errorf("summary carries a raw name: %q", ctx.Summary)
	}
	if !strings.Contains(ctx.Summary, strconv.Quote(name)) {
		t.Errorf("summary should quote the name: %q", ctx.Summary)
	}
	if !strings.Contains(ctx.Text, strconv.Quote("big.bin\nfake")+" (binary)") {
		t.Errorf("the not-attached list should quote the name:\n%q", ctx.Text)
	}
}

func TestBranchFilterRuleRemovedInWorkingTreeStillCounts(t *testing.T) {
	for name, mainAttrs := range map[string]string{
		"no attributes on main": "",
		"same file as main":     "*.txt text\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			run(t, dir, "init", "-q", "-b", "main")
			write(t, dir, "a.go", "package a\n")
			if mainAttrs != "" {
				write(t, dir, ".gitattributes", mainAttrs)
			}
			run(t, dir, "add", ".")
			run(t, dir, "commit", "-q", "-m", "init")
			run(t, dir, "checkout", "-q", "-b", "feature")
			// The branch commits the rule and the (encrypted) file...
			write(t, dir, ".gitattributes", mainAttrs+"vault.enc filter=crypt\n")
			write(t, dir, "vault.enc", "ciphertext\n")
			run(t, dir, "add", ".")
			run(t, dir, "commit", "-q", "-m", "vault")
			// ...and an uncommitted edit drops the rule; the copy is plaintext.
			write(t, dir, ".gitattributes", mainAttrs)
			write(t, dir, "vault.enc", "PLAINTEXT-VAULT\n")
			ctx, err := Diff(context.Background(), dir, "main", "")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(ctx.Text, "PLAINTEXT-VAULT") {
				t.Errorf("a file under HEAD's filter rule was sent:\n%s", ctx.Text)
			}
		})
	}
}

func TestFilterDriversNeverRunWithGitConfigSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell filter")
	}
	dir := newRepo(t)
	write(t, dir, ".gitattributes", "*.x filter=evil\n")
	write(t, dir, "a.x", "one\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "x")
	marker := filepath.Join(t.TempDir(), "ran")
	run(t, dir, "config", "filter.evil.clean", "sh -c 'touch "+marker+"; cat'")
	write(t, dir, "a.x", "two\n")
	// GIT_CONFIG redirects `git config` alone: discovery must not miss the
	// repo's driver because of it.
	t.Setenv("GIT_CONFIG", os.DevNull)
	if _, err := Diff(context.Background(), dir, "HEAD", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a filter driver ran with GIT_CONFIG set")
	}
	gitRunsDriver(t, dir, marker, "diff", "HEAD")
}

func TestGitNeverPromptsOrFetches(t *testing.T) {
	env := strings.Join(gitEnv(), "\n")
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("git's environment lacks %s", want)
		}
	}
}

func TestSubmoduleFiltersNeverRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell filter")
	}
	sub := newRepo(t)
	write(t, sub, ".gitattributes", "*.x filter=subf\n")
	write(t, sub, "a.x", "one\n")
	run(t, sub, "add", ".")
	run(t, sub, "commit", "-q", "-m", "x")
	dir := newRepo(t)
	run(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	run(t, dir, "commit", "-q", "-m", "sub")
	inner := filepath.Join(dir, "sub")
	marker := filepath.Join(t.TempDir(), "ran")
	// A driver in the submodule's own config, as git-crypt would write it.
	run(t, inner, "config", "filter.subf.clean", "sh -c 'touch "+marker+"; cat'")
	write(t, inner, "a.x", "two\n")
	write(t, dir, "a.go", "package a\n")

	if _, err := Diff(context.Background(), dir, "HEAD", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a submodule's filter driver ran during --diff")
	}
	// Positive control: plain git diff does run it, so the test is real.
	run(t, dir, "diff", "HEAD")
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this git does not run submodule filters here; nothing to check")
	}
}

// gitRunsDriver is a positive control for the "driver never ran" tests:
// plain git, run the same way on the same fixture, must create the marker,
// or the test would pass without checking anything.
func gitRunsDriver(t *testing.T, dir, marker string, args ...string) {
	t.Helper()
	os.Remove(marker)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Run() // a failing driver may make git fail; only the marker counts
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("control: plain git %v never ran the driver (%s), so this test checks nothing", args, filepath.Base(marker))
	}
	os.Remove(marker)
}

func TestRootRefusesAWorkTreeElsewhere(t *testing.T) {
	other := newRepo(t)
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	run(t, dir, "config", "core.worktree", other)
	if root, err := Root(context.Background(), dir); err == nil {
		t.Errorf("Root = %q; a core.worktree outside the folder should be refused", root)
	}
	if _, err := Diff(context.Background(), dir, "HEAD", ""); err == nil {
		t.Error("Diff should refuse it too")
	}
	sub := filepath.Join(other, "pkg")
	os.MkdirAll(sub, 0o755)
	if _, err := Root(context.Background(), sub); err != nil {
		t.Errorf("a subfolder of a normal repository: %v", err)
	}
}
