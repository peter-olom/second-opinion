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

// uninstallFixture installs so's files into a fake home and returns their paths.
func uninstallFixture(t *testing.T) (home, cfg, bin string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	for _, d := range []string{".claude/skills/second-opinion", ".claude/skills/other-skill", ".pi/agent/skills/second-opinion", "state/second-opinion/runs", "config/second-opinion", "bin"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg = filepath.Join(home, "config/second-opinion/config.toml")
	bin = filepath.Join(home, "bin/so")
	for _, f := range []string{cfg, bin, filepath.Join(home, "state/second-opinion/runs/x.json")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := executable
	executable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executable = orig })
	return home, cfg, bin
}

func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(input)
	w.Close()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
}

func gone(path string) bool {
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

func TestUninstallNeedsConfirmation(t *testing.T) {
	home, cfg, bin := uninstallFixture(t)
	for _, input := range []string{"", "n\n"} {
		withStdin(t, input)
		captureStdout(t, func() {
			captureStderr(t, func() {
				if code := run(context.Background(), []string{"uninstall"}); code == 0 {
					t.Errorf("input %q: exit code 0, want refusal", input)
				}
			})
		})
		for _, p := range []string{cfg, bin, filepath.Join(home, ".claude/skills/second-opinion")} {
			if gone(p) {
				t.Fatalf("input %q removed %s without confirmation", input, p)
			}
		}
	}
}

func TestUninstallKeepsConfigByDefault(t *testing.T) {
	home, cfg, bin := uninstallFixture(t)
	withStdin(t, "y\n")
	out := captureStdout(t, func() {
		if code := run(context.Background(), []string{"uninstall"}); code != 0 {
			t.Fatalf("exit code = %d", code)
		}
	})
	for _, p := range []string{
		filepath.Join(home, ".claude/skills/second-opinion"),
		filepath.Join(home, ".pi/agent/skills/second-opinion"),
		filepath.Join(home, "state/second-opinion"),
		bin,
	} {
		if !gone(p) {
			t.Errorf("%s was not removed", p)
		}
	}
	if gone(cfg) {
		t.Error("config should be kept without --all")
	}
	if gone(filepath.Join(home, ".claude/skills/other-skill")) {
		t.Error("other skills must not be touched")
	}
	if !strings.Contains(out, "Your config stays at") {
		t.Errorf("output should say the config is kept:\n%s", out)
	}
}

func TestUninstallAll(t *testing.T) {
	home, cfg, _ := uninstallFixture(t)
	captureStdout(t, func() {
		if code := run(context.Background(), []string{"uninstall", "--all", "--yes"}); code != 0 {
			t.Fatalf("exit code = %d", code)
		}
	})
	if !gone(cfg) || !gone(filepath.Join(home, "config/second-opinion")) {
		t.Error("--all should remove the config and its now-empty directory")
	}
}

func TestUninstallSkipsNonSoBinary(t *testing.T) {
	_, _, _ = uninstallFixture(t)
	other := filepath.Join(t.TempDir(), "so.test")
	os.WriteFile(other, []byte("x"), 0o755)
	executable = func() (string, error) { return other, nil }
	captureStdout(t, func() { run(context.Background(), []string{"uninstall", "--yes"}) })
	if gone(other) {
		t.Error("uninstall removed a binary that is not named so")
	}
}
