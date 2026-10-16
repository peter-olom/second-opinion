package backend

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStdoutCapIsEnforced(t *testing.T) {
	defer func(n int) { maxStdout = n }(maxStdout)
	maxStdout = 1000
	cli := script(t, "head -c 5000 /dev/zero | tr '\\0' x\n")
	_, err := run(context.Background(), "x", Options{Command: cli}, t.TempDir(), nil, "")
	if err == nil || !strings.Contains(err.Error(), "output exceeded") {
		t.Errorf("err = %v, want the stdout cap to apply", err)
	}
}

func TestStderrKeepsTheEnd(t *testing.T) {
	defer func(n int) { maxStderr = n }(maxStderr)
	maxStderr = 200
	// A long transcript, then the real error last.
	cli := script(t, "head -c 5000 /dev/zero | tr '\\0' x >&2; echo >&2; echo 'ERROR: 429 Too Many Requests' >&2; exit 1\n")
	_, err := run(context.Background(), "x", Options{Command: cli}, t.TempDir(), nil, "")
	if err == nil || !strings.Contains(err.Error(), "429 Too Many Requests") {
		t.Errorf("err = %v, want the final stderr line kept", err)
	}
}

func TestGuardCannotBeOverridden(t *testing.T) {
	cli := script(t, "echo \"$"+EnvGuard+"\"\n")
	out, err := run(context.Background(), "x", Options{Command: cli, Env: map[string]string{EnvGuard: ""}}, t.TempDir(), nil, "")
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Errorf("guard = %q, %v; want 1 even when config sets it", out, err)
	}
}

func TestLeftoverHelpersAreKilled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-only")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	cli := script(t, "sleep 60 & echo $! > "+pidFile+"; echo done\n")
	if _, err := run(context.Background(), "x", Options{Command: cli}, t.TempDir(), nil, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(pidFile)
	pid := strings.TrimSpace(string(data))
	time.Sleep(200 * time.Millisecond)
	if out, _ := os.ReadFile("/proc/" + pid + "/stat"); len(out) > 0 && !strings.Contains(string(out), " Z ") {
		t.Errorf("helper %s still running after the CLI exited", pid)
	}
	if runtime.GOOS == "darwin" {
		if p, err := os.FindProcess(atoi(pid)); err == nil && p.Signal(nil0()) == nil {
			t.Errorf("helper %s still running after the CLI exited", pid)
		}
	}
}

func TestCodexLockdown(t *testing.T) {
	features := []string{"apps", "browser_use", "shell_tool", "unified_exec", "code_mode_host", "enable_request_compression"}
	disabled := func(args []string) []string {
		var out []string
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--disable" {
				out = append(out, args[i+1])
			}
		}
		return out
	}

	noRepo := codexLockdown(features, false)
	for _, f := range []string{"apps", "browser_use", "shell_tool", "unified_exec", "code_mode_host"} {
		if !slices.Contains(disabled(noRepo), f) {
			t.Errorf("without a workspace, %s should be disabled: %v", f, noRepo)
		}
	}
	if slices.Contains(disabled(noRepo), "enable_request_compression") {
		t.Error("network features should be left alone")
	}
	if !slices.Contains(noRepo, `web_search="disabled"`) {
		t.Errorf("web search should be off: %v", noRepo)
	}

	repo := disabled(codexLockdown(features, true))
	for _, f := range []string{"shell_tool", "unified_exec", "code_mode_host"} {
		if slices.Contains(repo, f) {
			t.Errorf("with a workspace, %s is needed to read the repo: %v", f, repo)
		}
	}
	if !slices.Contains(repo, "apps") {
		t.Errorf("connected apps must be disabled with a workspace too: %v", repo)
	}
}

func TestParseCodexFeatures(t *testing.T) {
	out := "apps                       stable             true\nshell_tool  stable  true\nsqlite  removed  false\nsome banner line\n"
	got := parseCodexFeatures(out)
	if !slices.Equal(got, []string{"apps", "shell_tool", "sqlite"}) {
		t.Errorf("parseCodexFeatures = %v", got)
	}
}
