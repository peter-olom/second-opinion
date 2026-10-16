package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter-olom/second-opinion/internal/backend"
	"github.com/peter-olom/second-opinion/internal/config"
)

func TestAskRefusesInsidePanelist(t *testing.T) {
	t.Setenv(backend.EnvGuard, "1")
	if code := run(context.Background(), []string{"ask", "hello"}); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestResolveWorkspace(t *testing.T) {
	on, off := true, false
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	repo, _ = filepath.EvalSymlinks(repo)
	sub := filepath.Join(repo, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	plain, _ := filepath.EvalSymlinks(t.TempDir())

	tests := []struct {
		name        string
		dir         string
		explicit    bool
		disabled    bool
		modeDefault *bool
		want        string
	}{
		{"mode default in repo uses repo root", sub, false, false, &on, repo},
		{"mode without repo", sub, false, false, &off, ""},
		{"mode unset", sub, false, false, nil, ""},
		{"--no-repo overrides mode", sub, false, true, &on, ""},
		{"--repo in repo", sub, true, false, nil, repo},
		{"--repo outside repo uses cwd", plain, true, false, nil, plain},
		{"mode default outside repo grants nothing", plain, false, false, &on, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.dir)
			got, err := resolveWorkspace(tt.explicit, tt.disabled, tt.modeDefault)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("workspace = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSkillInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	// Only Claude Code and pi are "installed".
	for _, d := range []string{".claude", ".pi/agent"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	extra := filepath.Join(t.TempDir(), "skills")

	if code := run(context.Background(), []string{"skill", "install", "--dir", extra}); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	for _, dir := range []string{".claude/skills", ".pi/agent/skills"} {
		if _, err := os.Stat(filepath.Join(home, dir, "second-opinion", "SKILL.md")); err != nil {
			t.Errorf("skill not installed for detected harness: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Error("should not create directories for harnesses that are not installed")
	}
	if _, err := os.Stat(filepath.Join(extra, "second-opinion", "SKILL.md")); err != nil {
		t.Errorf("skill not installed into --dir: %v", err)
	}

	if code := run(context.Background(), []string{"skill", "install", "codex"}); code != 0 {
		t.Fatalf("explicit harness: exit code = %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex/skills/second-opinion/SKILL.md")); err != nil {
		t.Errorf("skill not installed for named harness: %v", err)
	}
	if code := run(context.Background(), []string{"skill", "install", "vim"}); code == 0 {
		t.Error("unknown harness should fail")
	}
}

func TestAskWithoutProfilesExplainsSetup(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	stderr := captureStderr(t, func() {
		if code := run(context.Background(), []string{"ask", "--config", cfg, "hello"}); code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
	})
	for _, want := range []string{"no profiles configured", "First run", "--with"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("error should mention %q:\n%s", want, stderr)
		}
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestAskRecordsRunAndRevealsLater(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-pi")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --model ] && model=$2; shift; done\ncat >/dev/null\necho \"opinion of $model\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.toml")
	toml := `
[backends.pi]
command = "` + fake + `"

[profiles.panel]
panelists = [{ backend = "pi", model = "alpha" }, { backend = "pi", model = "beta" }]
chair = { backend = "pi", model = "chair" }
`
	if err := os.WriteFile(cfg, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if code := run(context.Background(), []string{"ask", "--config", cfg, "--no-repo", "-q", "is this ok?"}); code != 0 {
			t.Fatalf("ask exit code = %d", code)
		}
	})
	if strings.Contains(out, "pi:alpha") || strings.Contains(out, "Panel key") {
		t.Errorf("ask output should be blind:\n%s", out)
	}
	if !strings.Contains(out, "run `") {
		t.Errorf("ask output should show the run ID:\n%s", out)
	}

	blind := captureStdout(t, func() { run(context.Background(), []string{"runs", "show", "last"}) })
	if strings.Contains(blind, "pi:alpha") {
		t.Errorf("runs show should stay blind without --reveal:\n%s", blind)
	}
	revealed := captureStdout(t, func() { run(context.Background(), []string{"runs", "show", "last", "--reveal"}) })
	for _, want := range []string{"## Panel key", "`pi:alpha`", "`pi:beta`", "- Chair: `pi:chair`"} {
		if !strings.Contains(revealed, want) {
			t.Errorf("runs show --reveal missing %q:\n%s", want, revealed)
		}
	}
	list := captureStdout(t, func() { run(context.Background(), []string{"runs"}) })
	if !strings.Contains(list, "panel") || !strings.Contains(list, "2/2 ok") || !strings.Contains(list, "is this ok?") {
		t.Errorf("runs list = %q", list)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	fn()
	os.Stdout = orig
	w.Close()
	return <-done
}

func loadConfigForTest(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
