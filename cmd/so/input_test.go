package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/peter-olom/second-opinion/internal/backend"
)

func TestStdinIsNotReadWhenThereIsAPrompt(t *testing.T) {
	withStdin(t, "LOOP-INPUT-THAT-MUST-NOT-LEAK\n")
	got, err := readPrompt([]string{"is", "this", "ok?"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "LOOP-INPUT") {
		t.Errorf("stdin was attached without being asked for: %q", got)
	}
}

func TestStdinWithTrailingDash(t *testing.T) {
	withStdin(t, "diff --git a/x b/x\n")
	got, err := readPrompt([]string{"review", "this", "-"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "review this") || !strings.Contains(got, "<stdin>\ndiff --git") {
		t.Errorf("prompt = %q, want the question plus stdin", got)
	}
}

func TestStdinAloneIsThePrompt(t *testing.T) {
	withStdin(t, "what about this?\n")
	got, err := readPrompt(nil, nil)
	if err != nil || got != "what about this?" {
		t.Errorf("prompt = %q, %v", got, err)
	}
}

func TestInputLimits(t *testing.T) {
	defer func(a, b int) { maxStdinBytes, maxFileBytes = a, b }(maxStdinBytes, maxFileBytes)
	maxStdinBytes, maxFileBytes = 10, 10

	withStdin(t, strings.Repeat("x", 11))
	if _, err := readPrompt(nil, nil); err == nil || !strings.Contains(err.Error(), "stdin is larger") {
		t.Errorf("oversized stdin: err = %v", err)
	}

	big := filepath.Join(t.TempDir(), "big.log")
	os.WriteFile(big, []byte(strings.Repeat("y", 11)), 0o644)
	if _, err := readPrompt([]string{"q"}, []string{big}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized -f: err = %v", err)
	}
}

func TestAttachRejectsFIFOWithoutHanging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs")
	}
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := mkfifo(fifo); err != nil {
		t.Skip(err)
	}
	// No writer ever appears: opening this FIFO would block forever.
	done := make(chan error, 1)
	go func() {
		_, err := readPrompt([]string{"q"}, []string{fifo})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %v, want a FIFO rejected", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a FIFO hung")
	}
}

func TestModeDefaultNeverExposesHome(t *testing.T) {
	on := true
	home := t.TempDir()
	if out, err := exec.Command("git", "-C", home, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sub := filepath.Join(home, "projects/x")
	os.MkdirAll(sub, 0o755)
	t.Chdir(sub)

	if got, err := resolveWorkspace(context.Background(), false, false, &on); err != nil || got != "" {
		t.Errorf("mode default in a repo rooted at $HOME = %q, %v; want no access", got, err)
	}
	// Explicit --repo from a project folder gets that folder, not all of $HOME.
	resolvedSub, _ := filepath.EvalSymlinks(sub)
	if got, err := resolveWorkspace(context.Background(), true, false, nil); err != nil || got != resolvedSub {
		t.Errorf("explicit --repo = %q, %v; want the current folder %q", got, err, resolvedSub)
	}
	// From home itself, explicit --repo is refused.
	t.Chdir(home)
	if got, err := resolveWorkspace(context.Background(), true, false, nil); err == nil {
		t.Errorf("explicit --repo from home = %q; want a refusal", got)
	}
}

func TestStdinFileNotReadGetsANote(t *testing.T) {
	f := filepath.Join(t.TempDir(), "input.txt")
	os.WriteFile(f, []byte("FILE-INPUT"), 0o644)
	in, _ := os.Open(f)
	orig := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = orig; in.Close() })

	var got string
	stderr := captureStderr(t, func() { got, _ = readPrompt([]string{"question"}, nil) })
	if strings.Contains(got, "FILE-INPUT") {
		t.Error("stdin was attached without a trailing -")
	}
	if !strings.Contains(stderr, "stdin was not read") {
		t.Errorf("expected a note about unread stdin, got %q", stderr)
	}
}

func TestPromptBudgetStopsBeforeReadingMore(t *testing.T) {
	defer func(n int) { maxPromptBytes = n }(maxPromptBytes)
	maxPromptBytes = 100
	dir := t.TempDir()
	var files []string
	for _, n := range []string{"a", "b", "c"} {
		p := filepath.Join(dir, n)
		os.WriteFile(p, []byte(strings.Repeat("x", 60)), 0o644)
		files = append(files, p)
	}
	if _, err := readPrompt([]string{"q"}, files); err == nil || !strings.Contains(err.Error(), "over the 100-byte limit") {
		t.Errorf("err = %v, want the budget to stop collection", err)
	}
}

func TestGuardRefusesEvenWhenEmpty(t *testing.T) {
	t.Setenv(backend.EnvGuard, "")
	if code := run(context.Background(), []string{"ask", "--config", fakePiConfig(t), "hello"}); code != 3 {
		t.Errorf("exit code = %d, want 3 for an empty but present guard", code)
	}
	if code := run(context.Background(), []string{"doctor", "--ping", "--config", filepath.Join(t.TempDir(), "c.toml"), "-w", "pi:x"}); code != 3 {
		t.Errorf("doctor --ping exit code = %d, want 3 inside a panelist", code)
	}
}

func TestNegativeTimeoutRejected(t *testing.T) {
	unsetEnv(t, backend.EnvGuard)
	captureStderr(t, func() {
		if code := run(context.Background(), []string{"ask", "--config", fakePiConfig(t), "--timeout", "-1s", "-w", "pi:x", "q"}); code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
	})
}

func TestPingWalksFallbacksOfRepeatedModels(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(cfgPath, []byte(`
[profiles.a]
panelists = [{ backend = "pi", model = "x", fallback = [{ backend = "pi", model = "fa" }] }]
[profiles.b]
panelists = [{ backend = "pi", model = "x", fallback = [{ backend = "pi", model = "fb" }] }]
`), 0o644)
	cfg := loadConfigForTest(t, cfgPath)
	targets, err := pingTargets(cfg, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, p := range targets {
		models = append(models, p.Model)
	}
	if strings.Join(models, ",") != "x,fa,fb" {
		t.Errorf("pinged %v, want x,fa,fb", models)
	}
}

func TestRepoRefusedFromHomeOutsideAnyRepo(t *testing.T) {
	home := t.TempDir() // not a git repository
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(home)
	if got, err := resolveWorkspace(context.Background(), true, false, nil); err == nil {
		t.Errorf("--repo from $HOME = %q; want a refusal", got)
	}
}

func TestCanaryReportsWhatModelsCanRead(t *testing.T) {
	unsetEnv(t, backend.EnvGuard)
	dir := t.TempDir()
	// A fake pi that reads any path named in its prompt, and one that cannot.
	reader := filepath.Join(dir, "reader")
	os.WriteFile(reader, []byte("#!/bin/sh\nprompt=$(cat)\nf=$(echo \"$prompt\" | grep -o '/[^ ]*canary.txt')\ncat \"$f\"\n"), 0o755)
	blind := filepath.Join(dir, "blind")
	os.WriteFile(blind, []byte("#!/bin/sh\ncat >/dev/null\necho NO-TOOLS\n"), 0o755)

	for name, tc := range map[string]struct {
		cli  string
		code int
		mark string
	}{"reads": {reader, 1, "✗"}, "blind": {blind, 0, "✓"}} {
		cfgPath := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(cfgPath, []byte("[backends.pi]\ncommand = \""+tc.cli+"\"\n"), 0o644)
		var code int
		out := captureStdout(t, func() {
			code = run(context.Background(), []string{"doctor", "--canary", "--config", cfgPath, "-w", "pi:fake"})
		})
		if code != tc.code || !strings.Contains(out, tc.mark+" pi:fake") {
			t.Errorf("%s: exit %d, output:\n%s\nwant exit %d and %s", name, code, out, tc.code, tc.mark)
		}
	}
}

func TestEmptyArgumentNeverReadsStdin(t *testing.T) {
	for _, arg := range []string{"", "   "} {
		withStdin(t, "LOOP-INPUT\n")
		got, err := readPrompt([]string{arg}, nil)
		if strings.Contains(got, "LOOP-INPUT") {
			t.Errorf("argument %q: stdin was read", arg)
		}
		if err == nil {
			t.Errorf("argument %q: want an empty-prompt error", arg)
		}
	}
}

func TestDiffRefusedInHomeRootedRepo(t *testing.T) {
	unsetEnv(t, backend.EnvGuard)
	home := t.TempDir()
	if out, err := exec.Command("git", "-C", home, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sub := filepath.Join(home, "notes")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(home, ".netrc"), []byte("password HOME-SECRET"), 0o600)
	t.Chdir(sub)
	stderr := captureStderr(t, func() {
		if code := run(context.Background(), []string{"ask", "--config", fakePiConfig(t), "--diff", "-w", "pi:x", "q"}); code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
	})
	if !strings.Contains(stderr, "rooted at your home directory") {
		t.Errorf("stderr = %q, want a refusal", stderr)
	}
}

func TestBoundedOutputDoesNotHangOnGrandchild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	cli := filepath.Join(t.TempDir(), "slow")
	os.WriteFile(cli, []byte("#!/bin/sh\nsleep 8 &\necho v1\nsleep 8\n"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	boundedOutput(ctx, cli, "--version")
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("boundedOutput took %s; a grandchild holding stdout must not hang it", d)
	}
}

func TestExplicitDashWithUnreadableStdinIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socketpair")
	}
	fds, err := syscallSocketpair()
	if err != nil {
		t.Skip(err)
	}
	sock := os.NewFile(uintptr(fds[0]), "sock")
	peer := os.NewFile(uintptr(fds[1]), "peer")
	peer.Write([]byte("SOCKET-DATA"))
	peer.Close()
	orig := os.Stdin
	os.Stdin = sock
	t.Cleanup(func() { os.Stdin = orig; sock.Close() })
	if _, err := readPrompt([]string{"q", "-"}, nil); err == nil || !strings.Contains(err.Error(), "not a pipe or file") {
		t.Errorf("err = %v; an explicit - must not silently drop socket stdin", err)
	}
}

func TestResolveWorkspaceHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := resolveWorkspace(ctx, true, false, nil); err == nil {
		t.Errorf("cancelled discovery returned %q, want the cancellation", got)
	}
}

// fakePiConfig is a config whose pi backend is a path that does not exist, so
// a test that regresses can never reach the developer's real pi.
func fakePiConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("[backends.pi]\ncommand = \"/nonexistent/so-test-pi\"\n"), 0o644)
	return p
}

func TestAnonymousFailureWithoutHistory(t *testing.T) {
	unsetEnv(t, backend.EnvGuard)
	dir := t.TempDir()
	fake := filepath.Join(dir, "pi")
	os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\necho 'pi: model secret-model-9 is broken' >&2\nexit 1\n"), 0o755)
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte("history = false\n[backends.pi]\ncommand = \""+fake+"\"\n"), 0o644)

	out := captureStdout(t, func() {
		captureStderr(t, func() {
			run(context.Background(), []string{"ask", "--config", cfg, "--no-repo", "-w", "pi:a", "-w", "pi:b", "q"})
		})
	})
	if strings.Contains(out, "secret-model-9") {
		t.Errorf("anonymous output leaked the backend's error:\n%s", out)
	}
	if strings.Contains(out, "so runs show") || !strings.Contains(out, "not recorded") {
		t.Errorf("with history off, the hint must not point to a run:\n%s", out)
	}
}

func TestProfilesRejectsUnknownBackend(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(cfg, []byte("[profiles.p]\npanelists = [{ backend = \"cladue\", model = \"opus\" }]\n"), 0o644)
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if code := run(context.Background(), []string{"profiles", "--config", cfg}); code == 0 {
				t.Error("so profiles accepted an unknown backend")
			}
		})
	})
	if !strings.Contains(stderr, "cladue") {
		t.Errorf("stderr = %q, want the bad backend named", stderr)
	}
}
