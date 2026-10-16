package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
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

	if got, err := resolveWorkspace(false, false, &on); err != nil || got != "" {
		t.Errorf("mode default in a repo rooted at $HOME = %q, %v; want no access", got, err)
	}
	resolvedHome, _ := filepath.EvalSymlinks(home)
	if got, _ := resolveWorkspace(true, false, nil); got != resolvedHome {
		t.Errorf("explicit --repo = %q, want %q (the user asked for it)", got, resolvedHome)
	}
}

func TestGuardRefusesEvenWhenEmpty(t *testing.T) {
	t.Setenv(backend.EnvGuard, "")
	if code := run(context.Background(), []string{"ask", "hello"}); code != 3 {
		t.Errorf("exit code = %d, want 3 for an empty but present guard", code)
	}
	if code := run(context.Background(), []string{"doctor", "--ping", "--config", filepath.Join(t.TempDir(), "c.toml"), "-w", "pi:x"}); code != 3 {
		t.Errorf("doctor --ping exit code = %d, want 3 inside a panelist", code)
	}
}

func TestNegativeTimeoutRejected(t *testing.T) {
	unsetEnv(t, backend.EnvGuard)
	captureStderr(t, func() {
		if code := run(context.Background(), []string{"ask", "--timeout", "-1s", "-w", "pi:x", "q"}); code != 1 {
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
