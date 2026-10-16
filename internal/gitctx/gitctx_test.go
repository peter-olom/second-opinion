package gitctx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<git-diff base="HEAD">`,
		"+func A() int { return 2 }",
		`<file path="a.go" status="changed">`,
		`<file path="new.go" status="new, untracked">`,
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

	ctx, err := Diff(context.Background(), dir, "main")
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
	if _, err := Diff(context.Background(), dir, "HEAD"); err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Errorf("clean tree: err = %v, want no changes", err)
	}
	if _, err := Diff(context.Background(), dir, "nope"); err == nil || !strings.Contains(err.Error(), "unknown base") {
		t.Errorf("bad base: err = %v", err)
	}
	if _, err := Diff(context.Background(), t.TempDir(), "HEAD"); err == nil || !strings.Contains(err.Error(), "git repository") {
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
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
		ctx, err := Diff(context.Background(), dir, "HEAD")
		done <- result{ctx.Text, err}
	}()
	select {
	case r := <-done:
		// Either git refuses the FIFO outright ("unsupported file type") or
		// it is listed as not attached; both are safe.
		if r.err == nil && !strings.Contains(r.text, "a.go (not a regular file)") {
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
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
	ctx, err := Diff(context.Background(), dir, "HEAD")
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Text, "DECRYPTED-SECRET") {
		t.Errorf("textconv output was attached:\n%s", ctx.Text)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the textconv driver ran")
	}
}

func TestDiffCancels(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Diff(ctx, dir, "HEAD"); err == nil {
		t.Error("a cancelled context should stop collection")
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PLAINTEXT-SECRET", "NEW-PLAINTEXT"} {
		if strings.Contains(ctx.Text, secret) {
			t.Errorf("filtered file content %s was sent:\n%s", secret, ctx.Text)
		}
	}
	if !strings.Contains(ctx.Text, "secrets.env (git filter or diff driver; may be encrypted)") {
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
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

	ctx, err := Diff(context.Background(), dir, "HEAD")
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
	ctx, err := Diff(context.Background(), dir, "HEAD")
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
	got := dropFiles(diff, map[string]bool{"drop me.txt": true, `we"ird.txt`: true})
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
	if _, err := Diff(context.Background(), dir, "HEAD"); err == nil || !strings.Contains(err.Error(), "too many changed files") {
		t.Errorf("err = %v, want a refusal rather than a partial check", err)
	}
}
