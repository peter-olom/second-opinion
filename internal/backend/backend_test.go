package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peter-olom/second-opinion/internal/harness"
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
	if atoi(pid) <= 0 {
		t.Fatalf("no helper PID recorded (%q)", pid)
	}
	time.Sleep(200 * time.Millisecond)
	if alive(atoi(pid)) {
		t.Errorf("helper %s still running after the CLI exited", pid)
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
	out := "apps                       stable             true\nshell_tool  stable  true\n\nsqlite  removed  false\n"
	got := parseCodexFeatures(out)
	if !slices.Equal(got, []string{"apps", "shell_tool", "sqlite"}) {
		t.Errorf("parseCodexFeatures = %v", got)
	}
	// A line in an unknown format could hide features: nothing is trusted.
	if got := parseCodexFeatures(out + "apps (stable): on\n"); got != nil {
		t.Errorf("an unparseable line should fail closed, got %v", got)
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
		{"claude, repo", claudeB, repo, []string{"--tools\nRead,Grep,Glob\n", "--strict-mcp-config", "--setting-sources\n\n", "--no-session-persistence", "--disable-slash-commands"}, nil},
		{"pi, no repo", piB, "", []string{"--no-tools", "--no-context-files", "--no-extensions", "--no-skills", "--no-approve", "--no-session", "--no-prompt-templates"}, []string{"--tools"}},
		{"pi, repo", piB, repo, []string{"--tools\nread,grep,find,ls\n", "--no-context-files", "--no-extensions", "--no-skills", "--no-approve", "--no-session", "--no-prompt-templates"}, []string{"--no-tools"}},
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

func TestPiAppendPromptIsAScratchFile(t *testing.T) {
	// pi reads --append-system-prompt as a file if one by that name exists,
	// relative to its cwd (the repository, with repo access). The value must
	// be an absolute path inside the private scratch folder, so no repo file
	// can stand in for it.
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, " "), []byte("INJECTED"), 0o644)
	os.WriteFile(filepath.Join(repo, piBasePrompt), []byte("INJECTED"), 0o644)
	scratch := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	cli := script(t, "printf '%s\\n' \"$@\" > "+argsFile+"\ncat >/dev/null\necho ok\n")
	pi{Options{Command: cli}}.Run(context.Background(), Request{System: "SECRET-MODE-INSTRUCTIONS", Prompt: "q", Dir: scratch, Workspace: repo})
	data, _ := os.ReadFile(argsFile)
	args := strings.Split(string(data), "\n")
	i := slices.Index(args, "--append-system-prompt")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no --append-system-prompt in %q", args)
	}
	value := args[i+1]
	if !filepath.IsAbs(value) || filepath.Dir(value) != scratch {
		t.Errorf("--append-system-prompt %q should be a file in the scratch folder %s", value, scratch)
	}
	// --system-prompt is read as a file the same way.
	j := slices.Index(args, "--system-prompt")
	if j < 0 || j+1 >= len(args) {
		t.Fatalf("no --system-prompt in %q", args)
	}
	if base := args[j+1]; !filepath.IsAbs(base) || filepath.Dir(base) != scratch {
		t.Errorf("--system-prompt %q should be a file in the scratch folder %s", base, scratch)
	} else if content, _ := os.ReadFile(base); strings.TrimSpace(string(content)) != piBasePrompt {
		t.Errorf("system prompt file = %q, want the base prompt", content)
	}
	if content, err := os.ReadFile(value); err != nil || strings.TrimSpace(string(content)) != "SECRET-MODE-INSTRUCTIONS" {
		t.Errorf("append file content = %q, %v; want the system prompt", content, err)
	}
	if info, err := os.Stat(value); err == nil && runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("append file mode = %v, want 0600", info.Mode().Perm())
	}
	// The system prompt stays out of argv, where other users could see it.
	if strings.Contains(string(data), "SECRET-MODE-INSTRUCTIONS") {
		t.Errorf("the system prompt is in pi's argv: %q", args)
	}
	if !slices.Contains(args, "--offline") {
		t.Errorf("pi should run with --offline: %q", args)
	}
}

func TestClaudeFailureUsesItsJSONResult(t *testing.T) {
	// Stderr has noise; the real error is in the JSON on stdout.
	cli := script(t, `cat >/dev/null
echo '(node:1) ExperimentalWarning: something' >&2
echo '{"type":"result","subtype":"error","is_error":true,"duration_ms":429,"result":"Claude AI usage limit reached"}'
exit 1`)
	_, err := claude{Options{Command: cli}}.Run(context.Background(), Request{Prompt: "q", Dir: t.TempDir()})
	if err == nil || !strings.HasSuffix(strings.TrimSpace(err.Error()), "Claude AI usage limit reached") {
		t.Errorf("err = %v; the JSON result should be the last line", err)
	}
}

func TestCodexArgv(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	argsFile := filepath.Join(t.TempDir(), "args")
	cli := script(t, `if [ "$1" = features ]; then printf 'apps  stable  true\nshell_tool  stable  true\n'; exit 0; fi
printf '%s\n' "$@" > `+argsFile+`
while [ $# -gt 0 ]; do [ "$1" = --output-last-message ] && out=$2; shift; done
cat >/dev/null; echo ok > "$out"`)
	for _, ws := range []string{"", t.TempDir()} {
		dir := t.TempDir()
		if _, err := (codex{Options{Command: cli}}).Run(context.Background(), Request{Prompt: "q", Dir: dir, Workspace: ws}); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(argsFile)
		args := string(data)
		cwd := dir
		if ws != "" {
			cwd = ws
		}
		for _, want := range []string{"--sandbox\nread-only\n", "--ignore-user-config\n", "--ignore-rules\n", "--ephemeral\n", "--cd\n" + cwd + "\n", "--disable\napps\n", `web_search="disabled"`, `shell_environment_policy.inherit="core"`, `shell_environment_policy.set={SECOND_OPINION_DEPTH="1"}`, "skills.max_context_tokens=1", "project_doc_max_bytes=0"} {
			if !strings.Contains(args, want) {
				t.Errorf("workspace %q: missing %q in:\n%s", ws, want, args)
			}
		}
		// The answer file stays in the private scratch folder, never the repo.
		if !strings.Contains(args, "--output-last-message\n"+filepath.Join(dir, "last-message.txt")+"\n") {
			t.Errorf("workspace %q: --output-last-message should be in the scratch folder %s:\n%s", ws, dir, args)
		}
		hasShellOff := strings.Contains(args, "--disable\nshell_tool\n")
		if (ws == "") != hasShellOff {
			t.Errorf("workspace %q: shell_tool disabled = %v", ws, hasShellOff)
		}
	}
}

func TestClaudeSuccessResultIsNotAnError(t *testing.T) {
	// A non-zero exit after a successful answer must not turn the answer into
	// the error text.
	cli := script(t, `cat >/dev/null
echo '{"type":"result","subtype":"success","is_error":false,"result":"A long answer that mentions rate limit handling."}'
exit 1`)
	_, err := claude{Options{Command: cli}}.Run(context.Background(), Request{Prompt: "q", Dir: t.TempDir()})
	if err == nil {
		t.Fatal("want an error for the non-zero exit")
	}
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	last := lines[len(lines)-1]
	if strings.Contains(last, "rate limit") || last != "claude: success: no error message" {
		t.Errorf("last line = %q; want the neutral line, not the answer", last)
	}
}

func TestCancelAndTimeoutKillTheCLIAndHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-only")
	}
	for _, tc := range []struct {
		name string
		ctx  func(pidFile string) (context.Context, context.CancelFunc)
		want error
	}{
		// Generous enough for the helper to start on a loaded machine.
		{"timeout", func(string) (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 2*time.Second)
		}, ErrTimeout},
		// Cancel once the helper exists, so the test always has one to check.
		{"cancel", func(pidFile string) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				for ctx.Err() == nil {
					if data, _ := os.ReadFile(pidFile); atoi(strings.TrimSpace(string(data))) > 0 {
						cancel()
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
			return ctx, cancel
		}, context.Canceled},
	} {
		pidFile := filepath.Join(t.TempDir(), "pid")
		cli := script(t, "sleep 30 & echo $! > "+pidFile+"\nsleep 30\n")
		ctx, cancel := tc.ctx(pidFile)
		start := time.Now()
		_, err := run(ctx, "x", Options{Command: cli}, t.TempDir(), nil, "")
		cancel()
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: took %s", tc.name, d)
		}
		time.Sleep(200 * time.Millisecond)
		data, _ := os.ReadFile(pidFile)
		pid := atoi(strings.TrimSpace(string(data)))
		if pid <= 0 {
			t.Fatalf("%s: helper pid not recorded", tc.name)
		}
		if alive(pid) {
			t.Errorf("%s: helper still running", tc.name)
		}
	}
}

func TestPiRefusesRepoItWouldMigrate(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".pi", "commands"), 0o755)
	os.WriteFile(filepath.Join(repo, ".pi", "commands", "old.md"), []byte("legacy"), 0o644)
	marker := filepath.Join(t.TempDir(), "pi-ran")
	cli := script(t, "touch "+marker+"\ncat >/dev/null\necho ok\n")
	_, err := pi{Options{Command: cli}}.Run(context.Background(), Request{System: "s", Prompt: "q", Dir: t.TempDir(), Workspace: repo})
	if err == nil || !strings.Contains(err.Error(), "would rename") {
		t.Errorf("err = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("pi was run in a repository it would migrate")
	}
	// With .pi/prompts present there is nothing to migrate, so pi runs.
	os.MkdirAll(filepath.Join(repo, ".pi", "prompts"), 0o755)
	if _, err := (pi{Options{Command: cli}}).Run(context.Background(), Request{System: "s", Prompt: "q", Dir: t.TempDir(), Workspace: repo}); err != nil {
		t.Errorf("with .pi/prompts present pi should run: %v", err)
	}
}

func TestFailureDetailIsBounded(t *testing.T) {
	cli := script(t, "head -c 200000 /dev/zero | tr '\\0' x\nexit 1\n")
	_, err := run(context.Background(), "x", Options{Command: cli}, t.TempDir(), nil, "")
	if err == nil || len(err.Error()) > maxDetailBytes+200 {
		t.Errorf("error is %d bytes; want it bounded near %d", len(err.Error()), maxDetailBytes)
	}
}

func TestCodexDiscoveryKeepsCancellation(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	cli := script(t, "sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := codexFeatures(ctx, Options{Command: cli})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestCodexDiscoveryTimeoutIsATimeout(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	cli := script(t, "sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := codexFeatures(ctx, Options{Command: cli})
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want it to wrap ErrTimeout", err)
	}
}

func TestClaudeMentionsAreDefused(t *testing.T) {
	for in, want := range map[string]string{
		"@/etc/passwd":                   "\u200b@/etc/passwd",
		"see @~/.aws/credentials please": "see \u200b@~/.aws/credentials please",
		"```\n@x\n```":                   "```\n\u200b@x\n```",
		"\u00a0@x (@y) \"@z\"":           "\u00a0\u200b@x (\u200b@y) \"\u200b@z\"",
		"mail me at a.b@example.com":     "mail me at a.b@example.com",
		"@Override\n\t@Test":             "\u200b@Override\n\t\u200b@Test",
		"+@/etc/passwd":                  "+\u200b@/etc/passwd",
		"-@x .@y _@z %@w":                "-\u200b@x .\u200b@y _\u200b@z %\u200b@w",
		"é@/etc/passwd":                  "é\u200b@/etc/passwd",
		"@@/x":                           "\u200b@\u200b@/x",
		"+\tsee @~/.ssh/id_rsa":          "+\tsee \u200b@~/.ssh/id_rsa",
	} {
		if got := defuseMentions(in); got != want {
			t.Errorf("defuseMentions(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClaudeGetsDefusedPromptAndReadDeny(t *testing.T) {
	dir := t.TempDir()
	argsFile, stdinFile := filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	cli := script(t, "printf '%s\\n' \"$@\" > "+argsFile+"\ncat > "+stdinFile+"\necho '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"ok\"}'\n")
	for _, workspace := range []string{"", t.TempDir()} {
		if _, err := (claude{Options{Command: cli}}).Run(context.Background(), Request{System: "s", Prompt: "read @/etc/passwd", Dir: t.TempDir(), Workspace: workspace, Deny: []string{"/repo/.state", "/work/[acme] api/so.toml"}}); err != nil {
			t.Fatal(err)
		}
		args, _ := os.ReadFile(argsFile)
		stdin, _ := os.ReadFile(stdinFile)
		if !strings.Contains(string(stdin), "\u200b@/etc/passwd") {
			t.Errorf("workspace %q: claude got an undefused mention: %q", workspace, stdin)
		}
		if !strings.Contains(string(args), strings.TrimSpace(mentionNote)) {
			t.Errorf("workspace %q: the system prompt lacks the note about defused mentions", workspace)
		}
		deny := strings.Contains(string(args), `{"permissions":{"deny":["Read"]}}`)
		if deny != (workspace == "") {
			t.Errorf("workspace %q: Read deny = %v, want it only without repo access", workspace, deny)
		}
		harnessDeny := true
		for _, rule := range []string{"Read(./.git)", "Read(./.git/**)", "Read(**/.git)", "Read(**/.git/**)"} {
			harnessDeny = harnessDeny && strings.Contains(string(args), strconv.Quote(rule))
		}
		for _, n := range harness.Names {
			for _, rule := range []string{"Read(./" + n + ")", "Read(./" + n + "/**)", "Read(**/" + n + ")", "Read(**/" + n + "/**)"} {
				harnessDeny = harnessDeny && strings.Contains(string(args), strconv.Quote(rule))
			}
		}
		harnessDeny = harnessDeny && strings.Contains(string(args), `"Read(//repo/.state/**)"`) && strings.Contains(string(args), `"Read(//repo/.state)"`) &&
			strings.Contains(string(args), `"Read(//work/\\[acme\\] api/so.toml)"`)
		if harnessDeny != (workspace != "") {
			t.Errorf("workspace %q: harness-state and .git deny = %v, want every rule with repo access", workspace, harnessDeny)
		}
	}
}

func TestConfigArgsComeAfterSoFlags(t *testing.T) {
	// README: args in a [backends.*] section are appended after so's flags.
	argsFile := filepath.Join(t.TempDir(), "args")
	cli := script(t, "printf '%s\\n' \"$@\" > "+argsFile+"\ncat >/dev/null\necho ok\n")
	if _, err := (pi{Options{Command: cli, Args: []string{"--user-flag"}}}).Run(context.Background(), Request{System: "s", Prompt: "q", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(argsFile)
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	if args[len(args)-1] != "--user-flag" || slices.Index(args, "--no-tools") > slices.Index(args, "--user-flag") {
		t.Errorf("config args should come last: %q", args)
	}
}

func TestClaudeAnswersLoseTheDefusingCharacter(t *testing.T) {
	cli := script(t, `cat >/dev/null
echo '{"type":"result","subtype":"success","result":"use \u200b@Override and \"\u200b@types/node\""}'`)
	out, err := claude{Options{Command: cli}}.Run(context.Background(), Request{Prompt: "q", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\u200b") || out != `use @Override and "@types/node"` {
		t.Errorf("answer = %q, want the inserted zero-width spaces removed", out)
	}
}

func TestPiMigrationGuardFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".pi", "commands"), 0o755)
	// A dangling prompts link: pi's existsSync says it is missing and renames
	// commands over it.
	os.Symlink(filepath.Join(repo, "nowhere"), filepath.Join(repo, ".pi", "prompts"))
	if !piWouldMigrate(repo) {
		t.Error("a dangling .pi/prompts should count as missing")
	}
	os.MkdirAll(filepath.Join(repo, "real"), 0o755)
	os.Remove(filepath.Join(repo, ".pi", "prompts"))
	os.Symlink(filepath.Join(repo, "real"), filepath.Join(repo, ".pi", "prompts"))
	if piWouldMigrate(repo) {
		t.Error("a .pi/prompts link to a folder counts as present")
	}
}

func TestPromptGoesOnlyThroughStdin(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	const sentinel = "PROMPT-SENTINEL-7319"
	for _, name := range Names() {
		dir := t.TempDir()
		argsFile, stdinFile := filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
		// Answers the way each CLI does: codex lists features and writes its
		// answer file, claude prints JSON, pi prints text.
		cli := script(t, `if [ "$1" = features ]; then printf 'apps  stable  true\n'; exit 0; fi
printf '%s\n' "$@" > `+argsFile+`
cat > `+stdinFile+`
while [ $# -gt 0 ]; do [ "$1" = --output-last-message ] && echo ok > "$2"; shift; done
echo '{"type":"result","subtype":"success","result":"ok"}'`)
		b, err := New(name, Options{Command: cli})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Run(context.Background(), Request{System: "s", Prompt: sentinel, Dir: t.TempDir()}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		args, _ := os.ReadFile(argsFile)
		stdin, _ := os.ReadFile(stdinFile)
		if strings.Contains(string(args), sentinel) || !strings.Contains(string(stdin), sentinel) {
			t.Errorf("%s: the prompt should be on stdin only; argv %q", name, args)
		}
	}
}

func TestDefusedMentionMakesNoPathToken(t *testing.T) {
	// "@" + U+200B + "/c" would mention a relative path "\u200b/c", which a
	// repository can provide (a folder named U+200B with a symlink out).
	// The marker must come before the @, leaving nothing after it.
	got := defuseMentions("see @/c")
	if strings.Contains(got, "@\u200b") || got != "see \u200b@/c" {
		t.Errorf("defuseMentions = %q, want the marker before the @", got)
	}
	if restoreMentions(got) != "see @/c" {
		t.Errorf("restoreMentions(%q) = %q", got, restoreMentions(got))
	}
}

func TestRipgrepConfigIsDropped(t *testing.T) {
	t.Setenv("RIPGREP_CONFIG_PATH", "evil.rgrc")
	envFile := filepath.Join(t.TempDir(), "env")
	cli := script(t, "env > "+envFile+"\ncat >/dev/null\necho ok\n")
	if _, err := run(context.Background(), "x", Options{Command: cli, Env: map[string]string{"RIPGREP_CONFIG_PATH": "also.rgrc", "KEEP": "1"}}, t.TempDir(), nil, ""); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(envFile)
	if strings.Contains(string(env), "RIPGREP_CONFIG_PATH") || !strings.Contains(string(env), "KEEP=1") {
		t.Errorf("RIPGREP_CONFIG_PATH should be dropped, other env kept:\n%s", env)
	}
}

func TestClaudeDropsLoneMarkers(t *testing.T) {
	cli := script(t, `cat >/dev/null
echo '{"type":"result","subtype":"success","result":"split \u200b and @x"}'`)
	out, err := claude{Options{Command: cli}}.Run(context.Background(), Request{Prompt: "see @x", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\u200b") {
		t.Errorf("a lone marker survived: %q", out)
	}
	// A request that holds zero-width spaces of its own keeps them.
	out, _ = claude{Options{Command: cli}}.Run(context.Background(), Request{Prompt: "own \u200b", Dir: t.TempDir()})
	if !strings.Contains(out, "\u200b") {
		t.Errorf("a zero-width space from the request itself was dropped: %q", out)
	}
}

func TestCodexDiscoveryUsesTheBackendEnv(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	envFile := filepath.Join(t.TempDir(), "env")
	cli := script(t, `echo "$CODEX_HOME" > `+envFile+`
printf 'apps  stable  true\n'`)
	if _, err := codexFeatures(context.Background(), Options{Command: cli, Env: map[string]string{"CODEX_HOME": "/moved/codex"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(envFile); strings.TrimSpace(string(got)) != "/moved/codex" {
		t.Errorf("codex features list ran with CODEX_HOME=%q, want the backend's", got)
	}
}

func TestGlobEscape(t *testing.T) {
	if got := globEscape(`/w/[a]*?\b`); got != `/w/\[a\]\*\?\\b` {
		t.Errorf("globEscape = %q", got)
	}
}

func TestCodexDiscoveryWaitersCanLeave(t *testing.T) {
	codexFeatureList = featureCache{}
	t.Cleanup(func() { codexFeatureList = featureCache{} })
	cli := script(t, "sleep 10\n") // a hung `codex features list`
	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	go codexFeatures(first, Options{Command: cli})
	time.Sleep(200 * time.Millisecond) // let it take the lock
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := codexFeatures(ctx, Options{Command: cli})
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a waiter took %s; it should give up when its own context ends", d)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
}
