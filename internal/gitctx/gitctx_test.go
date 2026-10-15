package gitctx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

	ctx, err := Diff(dir, "HEAD")
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

	ctx, err := Diff(dir, "main")
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
	if _, err := Diff(dir, "HEAD"); err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Errorf("clean tree: err = %v, want no changes", err)
	}
	if _, err := Diff(dir, "nope"); err == nil || !strings.Contains(err.Error(), "unknown base") {
		t.Errorf("bad base: err = %v", err)
	}
	if _, err := Diff(t.TempDir(), "HEAD"); err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Errorf("outside repo: err = %v", err)
	}
}
