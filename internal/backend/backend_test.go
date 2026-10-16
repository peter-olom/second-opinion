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

func TestCodexFailsClosedWithoutFeatureList(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	// A codex that cannot list its features must not be run with tools unlocked.
	marker := filepath.Join(t.TempDir(), "ran-exec")
	cli := script(t, `if [ "$1" = features ]; then echo "unknown command" >&2; exit 2; fi; touch `+marker+`; echo answer`)
	_, err := codex{Options{Command: cli}}.Run(context.Background(), Request{Prompt: "q", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "cannot list its features") {
		t.Errorf("err = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("codex exec ran without its tools locked down")
	}
}

func TestHelperHoldingStdinDoesNotLoseTheAnswer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-only")
	}
	// The CLI answers without reading its (large) prompt, while a helper keeps
	// stdin open. The answer must come back promptly and without error.
	cli := script(t, "sleep 30 <&0 & echo answer\n")
	start := time.Now()
	out, err := run(context.Background(), "x", Options{Command: cli}, t.TempDir(), nil, strings.Repeat("p", 1<<20))
	if err != nil || strings.TrimSpace(string(out)) != "answer" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("took %s; the stdin writer should not wait on the helper", d)
	}
}

func TestTailBufferManySmallWrites(t *testing.T) {
	b := &tailBuffer{limit: 100}
	var all strings.Builder
	for i := range 10000 {
		line := strings.Repeat(string(rune('a'+i%26)), 7) + "\n"
		b.Write([]byte(line))
		all.WriteString(line)
	}
	want := all.String()[all.Len()-100:]
	if got := b.String(); got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
	if len(b.buf) > 200 {
		t.Errorf("buffer grew to %d bytes, want at most twice the limit", len(b.buf))
	}
}

func TestCodexFeatureListRetriedAfterFailure(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	flag := filepath.Join(t.TempDir(), "ready")
	// The first listing fails; once the flag exists, it works.
	cli := script(t, `if [ "$1" = features ]; then [ -f `+flag+` ] && echo "apps  stable  true" && exit 0; exit 1; fi; echo answer`)
	o := Options{Command: cli}
	if _, err := codexFeatures(context.Background(), o); err == nil {
		t.Fatal("first listing should fail")
	}
	os.WriteFile(flag, nil, 0o644)
	if names, err := codexFeatures(context.Background(), o); err != nil || len(names) != 1 {
		t.Errorf("second listing = %v, %v; a failure must not be cached", names, err)
	}
}

// argvOf runs a backend with a fake CLI that records its arguments.
func argvOf(t *testing.T, b func(Options) Backend, workspace string) string {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "args")
	cli := script(t, "printf '%s\\n' \"$@\" > "+argsFile+"\ncat >/dev/null\necho '{\"result\":\"ok\"}'\n")
	b(Options{Command: cli}).Run(context.Background(), Request{System: "sys", Prompt: "q", Dir: t.TempDir(), Workspace: workspace})
	data, _ := os.ReadFile(argsFile)
	return string(data)
}

func TestClaudeAndPiLockdownFlags(t *testing.T) {
	repo := t.TempDir()
	claudeB := func(o Options) Backend { return claude{o} }
	piB := func(o Options) Backend { return pi{o} }
	for _, tc := range []struct {
		name      string
		b         func(Options) Backend
		workspace string
		want      []string
		notWant   []string
	}{
		{"claude, no repo", claudeB, "", []string{"--tools\n\n", "--strict-mcp-config", "--setting-sources\n\n", "--no-session-persistence", "--disable-slash-commands"}, []string{"Read,Grep,Glob"}},
		{"claude, repo", claudeB, repo, []string{"--tools\nRead,Grep,Glob\n", "--strict-mcp-config", "--setting-sources\n\n"}, nil},
		{"pi, no repo", piB, "", []string{"--no-tools", "--no-context-files", "--no-extensions", "--no-skills", "--no-approve", "--no-session"}, []string{"--tools"}},
		{"pi, repo", piB, repo, []string{"--tools\nread,grep,find,ls\n", "--no-extensions", "--no-skills", "--no-approve"}, []string{"--no-tools"}},
	} {
		args := argvOf(t, tc.b, tc.workspace)
		for _, w := range tc.want {
			if !strings.Contains(args, w) {
				t.Errorf("%s: missing %q in:\n%s", tc.name, w, args)
			}
		}
		for _, nw := range tc.notWant {
			if strings.Contains(args, nw) {
				t.Errorf("%s: unexpected %q in:\n%s", tc.name, nw, args)
			}
		}
	}
}

func TestCodexFinalMessageIsBounded(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	defer func(n int) { maxStdout = n }(maxStdout)
	maxStdout = 100
	// A codex that writes a huge final message to --output-last-message.
	cli := script(t, `if [ "$1" = features ]; then echo "apps stable true"; exit 0; fi
while [ $# -gt 0 ]; do [ "$1" = --output-last-message ] && out=$2; shift; done
head -c 5000 /dev/zero | tr '\0' x > "$out"`)
	_, err := codex{Options{Command: cli}}.Run(context.Background(), Request{Prompt: "q", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "output exceeded") {
		t.Errorf("err = %v, want the final message bounded", err)
	}
}
