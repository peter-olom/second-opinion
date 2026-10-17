package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/peter-olom/second-opinion/internal/backend"
	"github.com/peter-olom/second-opinion/internal/history"
)

type fixture struct {
	home, cfg, bin, state string
}

// newFixture installs so's files into a fake home. Every location so derives
// from the environment points inside it, so these tests cannot touch the real
// machine even if the developer has SECOND_OPINION_CONFIG or XDG variables set.
func newFixture(t *testing.T) fixture {
	t.Helper()
	home := t.TempDir()
	f := fixture{
		home:  home,
		cfg:   filepath.Join(home, "config/second-opinion/config.toml"),
		bin:   filepath.Join(home, "bin/so"),
		state: filepath.Join(home, "state/second-opinion"),
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("SECOND_OPINION_CONFIG", "")
	t.Setenv("PATH", filepath.Join(home, "bin"))
	unsetEnv(t, backend.EnvGuard)

	for _, d := range []string{".claude/skills/second-opinion", ".claude/skills/other-skill", ".pi/agent/skills/second-opinion", "state/second-opinion/runs", "config/second-opinion", "bin"} {
		mkdir(t, filepath.Join(home, d))
	}
	for _, p := range []string{
		f.cfg, f.bin,
		filepath.Join(home, ".claude/skills/second-opinion/SKILL.md"),
		filepath.Join(home, ".claude/skills/other-skill/SKILL.md"),
		filepath.Join(home, ".pi/agent/skills/second-opinion/SKILL.md"),
		filepath.Join(f.state, "runs/20261015-120000-abcd.json"),
	} {
		write(t, p)
	}
	os.Chmod(f.bin, 0o755)

	origExe, origInvoked, origGOOS := executable, invokedAs, goos
	executable = func() (string, error) { return f.bin, nil }
	invokedAs = func() string { return f.bin }
	t.Cleanup(func() { executable, invokedAs, goos = origExe, origInvoked, origGOOS })
	return f
}

// uninstall runs `so uninstall` against the fixture's config with the given stdin.
func (f fixture) uninstall(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	withStdin(t, stdin)
	args = append([]string{"uninstall", "--config", f.cfg}, args...)
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() { code = run(context.Background(), args) })
	})
	return code, stdout, stderr
}

// uninstallDefaultConfig runs `so uninstall` without --config, so the config
// path comes from the environment.
func uninstallDefaultConfig(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	withStdin(t, "")
	args = append([]string{"uninstall"}, args...)
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() { code = run(context.Background(), args) })
	})
	return code, stdout, stderr
}

// mustNotSayRemoved fails if the output claims dir itself was removed.
func mustNotSayRemoved(t *testing.T, out, dir string) {
	t.Helper()
	if strings.Contains(out, "removed "+dir+"\n") {
		t.Errorf("output claims %s was removed, but it was kept:\n%s", dir, out)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
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

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func mustExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if !exists(p) {
			t.Errorf("%s was removed but should survive", p)
		}
	}
}

func mustBeGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if exists(p) {
			t.Errorf("%s should have been removed", p)
		}
	}
}

func skipIfRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission tests are meaningless as root")
	}
}

func TestUninstallNeedsConfirmation(t *testing.T) {
	for _, input := range []string{"", "n\n", "y n\n", "yes please\n"} {
		f := newFixture(t)
		code, _, _ := f.uninstall(t, input)
		if code == 0 {
			t.Errorf("input %q: exit code 0, want refusal", input)
		}
		mustExist(t, f.cfg, f.bin, f.state, filepath.Join(f.home, ".claude/skills/second-opinion/SKILL.md"))
	}
}

func TestUninstallAcceptsYesWithoutNewline(t *testing.T) {
	f := newFixture(t)
	if code, _, stderr := f.uninstall(t, "y"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, f.bin)
}

func TestUninstallKeepsConfigByDefault(t *testing.T) {
	f := newFixture(t)
	code, out, stderr := f.uninstall(t, "y\n")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t,
		filepath.Join(f.home, ".claude/skills/second-opinion"),
		filepath.Join(f.home, ".pi/agent/skills/second-opinion"),
		f.state, f.bin)
	mustExist(t, f.cfg, filepath.Join(f.home, ".claude/skills/other-skill/SKILL.md"))
	resolved, _ := filepath.EvalSymlinks(f.cfg)
	if !strings.Contains(out, "Your config stays at "+resolved) {
		t.Errorf("output should say the config is kept:\n%s", out)
	}
	if !strings.Contains(out, "1 run") {
		t.Errorf("listing should count the runs:\n%s", out)
	}
}

func TestUninstallAll(t *testing.T) {
	f := newFixture(t)
	if code, _, stderr := f.uninstall(t, "", "--all", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, f.cfg, filepath.Dir(f.cfg))
}

func TestUninstallRefusesInsidePanelist(t *testing.T) {
	f := newFixture(t)
	t.Setenv(backend.EnvGuard, "1")
	if code, _, _ := f.uninstall(t, "", "--all", "--yes"); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	mustExist(t, f.cfg, f.bin, f.state)
}

func TestUninstallKeepsConfigSharingTheStateDirectory(t *testing.T) {
	// XDG_STATE_HOME == XDG_CONFIG_HOME puts the config inside the history folder.
	f := newFixture(t)
	shared := filepath.Join(f.home, "shared")
	t.Setenv("XDG_STATE_HOME", shared)
	t.Setenv("XDG_CONFIG_HOME", shared)
	mkdir(t, filepath.Join(shared, "second-opinion/runs"))
	run := filepath.Join(shared, "second-opinion/runs/20261015-120000-abcd.json")
	cfg := filepath.Join(shared, "second-opinion/config.toml")
	write(t, run)
	write(t, cfg)
	f.cfg = cfg

	code, out, stderr := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, run, filepath.Join(shared, "second-opinion/runs"))
	mustExist(t, cfg)
	dir := filepath.Join(shared, "second-opinion")
	if !strings.Contains(out, "removed so's files from "+dir+"; kept the folder") {
		t.Errorf("output should explain the folder was kept:\n%s", out)
	}
	mustNotSayRemoved(t, out, dir)
}

func TestUninstallAllWithSharedStateAndConfigFolder(t *testing.T) {
	f := newFixture(t)
	shared := filepath.Join(f.home, "shared")
	t.Setenv("XDG_STATE_HOME", shared)
	t.Setenv("XDG_CONFIG_HOME", shared)
	dir := filepath.Join(shared, "second-opinion")
	mkdir(t, filepath.Join(dir, "runs"))
	write(t, filepath.Join(dir, "runs/20261015-120000-abcd.json"))
	f.cfg = filepath.Join(dir, "config.toml")
	write(t, f.cfg)

	code, out, stderr := f.uninstall(t, "", "--all", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, dir)
	if strings.Contains(out, "kept") {
		t.Errorf("nothing was kept, but the output says so:\n%s", out)
	}
}

func TestUninstallIgnoresRelativeXDG(t *testing.T) {
	newFixture(t)
	// The current directory holds files shaped exactly like so's state and
	// config. With relative XDG values honoured, uninstall would delete them.
	cwd := t.TempDir()
	mkdir(t, filepath.Join(cwd, "second-opinion/runs"))
	run := filepath.Join(cwd, "second-opinion/runs/20261015-120000-abcd.json")
	cfg := filepath.Join(cwd, "second-opinion/config.toml")
	write(t, run)
	write(t, cfg)
	t.Chdir(cwd)
	t.Setenv("XDG_STATE_HOME", ".")
	t.Setenv("XDG_CONFIG_HOME", ".")

	if code, _, stderr := uninstallDefaultConfig(t, "--all", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustExist(t, run, cfg)
}

func TestUninstallKeepsUserFilesInSkillFolder(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.home, ".claude/skills/second-opinion")
	mine := filepath.Join(dir, "my-notes.md")
	write(t, mine)

	code, out, _ := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	mustBeGone(t, filepath.Join(dir, "SKILL.md"))
	mustExist(t, mine)
	if !strings.Contains(out, "removed so's files from "+dir+"; kept the folder") {
		t.Errorf("output should explain the folder was kept:\n%s", out)
	}
	mustNotSayRemoved(t, out, dir)
}

func TestUninstallLeavesSymlinkedFoldersAlone(t *testing.T) {
	f := newFixture(t)
	// The pi skill folder is a symlink into a dotfiles repo.
	dotfiles := filepath.Join(t.TempDir(), "pi-skill")
	mkdir(t, dotfiles)
	write(t, filepath.Join(dotfiles, "SKILL.md"))
	link := filepath.Join(f.home, ".pi/agent/skills/second-opinion")
	os.RemoveAll(link)
	if err := os.Symlink(dotfiles, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	// The config folder is a symlink too.
	cfgTarget := filepath.Join(t.TempDir(), "cfg")
	mkdir(t, cfgTarget)
	write(t, filepath.Join(cfgTarget, "config.toml"))
	write(t, filepath.Join(cfgTarget, "other.toml"))
	cfgDir := filepath.Dir(f.cfg)
	os.RemoveAll(cfgDir)
	if err := os.Symlink(cfgTarget, cfgDir); err != nil {
		t.Fatal(err)
	}

	code, out, stderr := f.uninstall(t, "", "--all", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustExist(t, link, filepath.Join(dotfiles, "SKILL.md"), cfgDir, filepath.Join(cfgTarget, "other.toml"))
	mustBeGone(t, filepath.Join(cfgTarget, "config.toml"))
	if !strings.Contains(out, "left "+link+" alone") {
		t.Errorf("output should say the symlinked folder was left alone:\n%s", out)
	}
}

func TestUninstallRemovesOnlyTheBinaryLink(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(t.TempDir(), "build/so")
	mkdir(t, filepath.Dir(target))
	write(t, target)
	os.Chmod(target, 0o755) // PATH lookup only finds executables
	link := filepath.Join(f.home, "bin/so")
	os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	executable = func() (string, error) { return target, nil } // what os.Executable reports
	invokedAs = func() string { return "so" }                  // found through PATH

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, link)
	mustExist(t, target)
}

func TestUninstallSkipsNonSoBinary(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(t.TempDir(), "so.test")
	write(t, other)
	executable = func() (string, error) { return other, nil }
	invokedAs = func() string { return other }
	code, out, _ := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Errorf("exit code = %d", code)
	}
	mustExist(t, other)
	if strings.Contains(out, "  binary") {
		t.Errorf("listing should not include the binary:\n%s", out)
	}
	if !strings.Contains(out, "was not removed because it is not named so") {
		t.Errorf("output should say the binary was skipped:\n%s", out)
	}
}

func TestUninstallRemovesLinkToVersionedBinary(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(t.TempDir(), "so-v0.1.0")
	write(t, target)
	os.Chmod(target, 0o755)
	link := filepath.Join(f.home, "bin/so")
	os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	executable = func() (string, error) { return target, nil }
	invokedAs = func() string { return "so" }

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, link)
	mustExist(t, target)
}

func TestUninstallNotesAnotherSoOnPath(t *testing.T) {
	f := newFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "other-bin")
	mkdir(t, elsewhere)
	write(t, filepath.Join(elsewhere, "so"))
	os.Chmod(filepath.Join(elsewhere, "so"), 0o755)
	t.Setenv("PATH", filepath.Join(f.home, "bin")+string(os.PathListSeparator)+elsewhere)

	_, out, _ := f.uninstall(t, "", "--yes")
	if !strings.Contains(out, "another so is still on your PATH at "+filepath.Join(elsewhere, "so")) {
		t.Errorf("output should mention the other so:\n%s", out)
	}
	mustExist(t, filepath.Join(elsewhere, "so"))
}

func TestUninstallThroughRelativePathEntry(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(t.TempDir(), "build/so")
	mkdir(t, filepath.Dir(target))
	write(t, target)
	os.Chmod(target, 0o755)
	link := filepath.Join(f.home, "bin/so")
	os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	t.Chdir(f.home)
	t.Setenv("PATH", "bin") // relative: exec.LookPath reports exec.ErrDot
	executable = func() (string, error) { return target, nil }
	invokedAs = func() string { return "so" }

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, link)
	mustExist(t, target)
}

func TestUninstallRemovesDifferentlyNamedLink(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(t.TempDir(), "build/so")
	mkdir(t, filepath.Dir(target))
	write(t, target)
	link := filepath.Join(f.home, "bin/so-dev")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	executable = func() (string, error) { return target, nil }
	invokedAs = func() string { return link }

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, link)
	mustExist(t, target)
}

func TestUninstallSkipsFoldersWithNothingOfSos(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.home, ".claude/skills/second-opinion")
	os.Remove(filepath.Join(dir, "SKILL.md"))
	mine := filepath.Join(dir, "my-notes.md")
	write(t, mine)

	code, out, _ := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	mustExist(t, mine)
	if strings.Contains(out, "claude skill") || strings.Contains(out, "removed so's files from "+dir) {
		t.Errorf("a folder with nothing of so's should not be listed or reported as removed:\n%s", out)
	}
	if !strings.Contains(out, "left "+dir+" alone: there is nothing of so's to remove") {
		t.Errorf("output should explain the folder was left alone:\n%s", out)
	}
}

func TestUninstallCancelledAtPrompt(t *testing.T) {
	f := newFixture(t)
	// stdin stays open with no answer, like a user who presses Ctrl-C.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { w.Close(); os.Stdin = orig })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var code int
	out := captureStdout(t, func() {
		code = run(ctx, []string{"uninstall", "--config", f.cfg})
	})
	if code != 130 || !strings.Contains(out, "cancelled; nothing was removed") {
		t.Errorf("exit code = %d, output = %q; want 130 and a cancellation message", code, out)
	}
	mustExist(t, f.cfg, f.bin, f.state, filepath.Join(f.home, ".claude/skills/second-opinion/SKILL.md"))
}

func TestUninstallAllResolvesDotDotLikeTheFilesystem(t *testing.T) {
	f := newFixture(t)
	// link/.. is home/srv, not home: the real config lives at home/srv/config.toml.
	nested := filepath.Join(f.home, "srv/nested")
	mkdir(t, nested)
	link := filepath.Join(f.home, "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	real := filepath.Join(f.home, "srv/config.toml")
	decoy := filepath.Join(f.home, "config.toml")
	write(t, real)
	write(t, decoy)
	f.cfg = link + "/../config.toml"

	if code, _, stderr := f.uninstall(t, "", "--all", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, real)
	mustExist(t, decoy)
}

func TestUninstallKeepsHardlinkedConfig(t *testing.T) {
	f := newFixture(t)
	// The config is also reachable as a run file. Without --all it must survive.
	alias := filepath.Join(f.state, "runs/20261015-130000-cf00.json")
	if !history.IsRunFile(filepath.Base(alias)) {
		t.Fatal("the alias must look like a run record, or uninstall would never consider it")
	}
	if err := os.Link(f.cfg, alias); err != nil {
		t.Skip("hard links unavailable:", err)
	}
	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustExist(t, f.cfg, alias)
}

func TestUninstallMovesRunningBinaryAsideOnWindows(t *testing.T) {
	f := newFixture(t)
	goos = "windows"
	code, out, stderr := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, f.bin)
	mustExist(t, f.bin+".old")
	if !strings.Contains(out, "delete it once this command exits") {
		t.Errorf("output should tell the user to delete the old binary:\n%s", out)
	}
}

func TestUninstallNeverOverwritesOldBinaryOnWindows(t *testing.T) {
	f := newFixture(t)
	goos = "windows"
	if err := os.WriteFile(f.bin+".old", []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	if data, _ := os.ReadFile(f.bin + ".old"); string(data) != "precious" {
		t.Errorf("existing %s.old was overwritten", f.bin)
	}
	mustExist(t, f.bin+".old.1")
	mustBeGone(t, f.bin)
}

func TestUninstallRefusesDirectoryConfig(t *testing.T) {
	f := newFixture(t)
	f.cfg = filepath.Join(f.home, "config/second-opinion")
	code, _, stderr := f.uninstall(t, "", "--all", "--yes")
	if code != 1 || !strings.Contains(stderr, "is a directory") {
		t.Errorf("exit code = %d, stderr = %q; want a refusal", code, stderr)
	}
	mustExist(t, f.bin, f.state)
}

func TestUninstallLeavesSymlinkedRunsAlone(t *testing.T) {
	f := newFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "runs")
	mkdir(t, elsewhere)
	precious := filepath.Join(elsewhere, "20261015-120000-ffff.json")
	write(t, precious)
	runs := filepath.Join(f.state, "runs")
	os.RemoveAll(runs)
	if err := os.Symlink(elsewhere, runs); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustExist(t, precious, runs)
}

func TestUninstallReportsPartialFailure(t *testing.T) {
	skipIfRoot(t)
	f := newFixture(t)
	locked := filepath.Join(f.home, ".pi/agent/skills/second-opinion")
	os.Chmod(locked, 0o500) // SKILL.md cannot be deleted
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	code, _, stderr := f.uninstall(t, "", "--yes")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "could not remove") {
		t.Errorf("stderr should report the failure:\n%s", stderr)
	}
	mustExist(t, filepath.Join(locked, "SKILL.md"))
	mustBeGone(t, filepath.Join(f.home, ".claude/skills/second-opinion"), f.state, f.bin)
}

func TestUninstallAbortsOnUnreadableFolder(t *testing.T) {
	skipIfRoot(t)
	f := newFixture(t)
	// Searchable but not readable: Lstat of children works, ReadDir does not.
	dir := filepath.Join(f.home, ".pi/agent/skills/second-opinion")
	os.Chmod(dir, 0o300)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	code, _, stderr := f.uninstall(t, "", "--yes")
	if code != 1 || !strings.Contains(stderr, "nothing was removed") {
		t.Errorf("exit code = %d, stderr = %q; want an abort before deleting anything", code, stderr)
	}
	mustExist(t, filepath.Join(dir, "SKILL.md"), f.bin, f.state)
}

func TestUninstallAllAbortsOnUnreadableConfigFolder(t *testing.T) {
	skipIfRoot(t)
	f := newFixture(t)
	// Searchable but not readable: lstat of the config works, ReadDir does not,
	// so only the pre-delete ReadDir check can stop this run.
	dir := filepath.Dir(f.cfg)
	os.Chmod(dir, 0o300)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	code, _, stderr := f.uninstall(t, "", "--all", "--yes")
	if code != 1 || !strings.Contains(stderr, "nothing was removed") {
		t.Errorf("exit code = %d, stderr = %q; want an abort before deleting anything", code, stderr)
	}
	mustExist(t, f.bin, f.state)
}

func TestUninstallAbortsOnUnreadablePaths(t *testing.T) {
	skipIfRoot(t)
	f := newFixture(t)
	skills := filepath.Join(f.home, ".claude/skills")
	os.Chmod(skills, 0o000)
	t.Cleanup(func() { os.Chmod(skills, 0o755) })

	code, _, stderr := f.uninstall(t, "", "--yes")
	if code != 1 || !strings.Contains(stderr, "nothing was removed") {
		t.Errorf("exit code = %d, stderr = %q; want an abort before deleting anything", code, stderr)
	}
	mustExist(t, f.bin, f.state, filepath.Join(f.home, ".pi/agent/skills/second-opinion/SKILL.md"))
}

func TestUninstallAllRefusesUnresolvableDotDot(t *testing.T) {
	skipIfRoot(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string) string // returns the component before ".."
	}{
		{"missing", func(t *testing.T, home string) string { return filepath.Join(home, "missing") }},
		{"file", func(t *testing.T, home string) string {
			p := filepath.Join(home, "plainfile")
			write(t, p)
			return p
		}},
		{"unreadable", func(t *testing.T, home string) string {
			p := filepath.Join(home, "locked/inner")
			mkdir(t, p)
			os.Chmod(filepath.Dir(p), 0o000)
			t.Cleanup(func() { os.Chmod(filepath.Dir(p), 0o755) })
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			decoy := filepath.Join(f.home, "config.toml")
			write(t, decoy)
			f.cfg = tc.setup(t, f.home) + "/../config.toml"

			code, _, stderr := f.uninstall(t, "", "--all", "--yes")
			if code != 1 || !strings.Contains(stderr, "nothing was removed") {
				t.Errorf("exit code = %d, stderr = %q; want an abort", code, stderr)
			}
			mustExist(t, decoy, f.bin, f.state)
		})
	}
}

func TestUninstallAllWithMissingConfigFolder(t *testing.T) {
	f := newFixture(t)
	f.cfg = filepath.Join(f.home, "nowhere/second-opinion/config.toml")
	if code, _, stderr := f.uninstall(t, "", "--all", "--yes"); code != 0 {
		t.Fatalf("a config in a missing folder is just absent; exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, f.bin)
}

func TestResolveConfigPathKeepsDriveRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive letters only exist on Windows")
	}
	got, _, err := resolveConfigPath(`C:\config.toml`)
	if err != nil || !strings.EqualFold(got, `C:\config.toml`) {
		t.Errorf("resolveConfigPath = %q, %v; want C:\\config.toml", got, err)
	}
}

func TestUninstallKeepsConfigSpelledThroughSymlink(t *testing.T) {
	f := newFixture(t)
	// The config path is a symlink to a run file. Without --all that file is
	// the user's config and must survive, even though it looks like a run.
	run := filepath.Join(f.state, "runs/20261015-120000-abcd.json")
	f.cfg = filepath.Join(f.home, "config-link.toml")
	if err := os.Symlink(run, f.cfg); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustExist(t, run, f.cfg)
}

func TestUninstallClearsEmptyRunsInSharedFolder(t *testing.T) {
	f := newFixture(t)
	shared := filepath.Join(f.home, "shared")
	t.Setenv("XDG_STATE_HOME", shared)
	t.Setenv("XDG_CONFIG_HOME", shared)
	dir := filepath.Join(shared, "second-opinion")
	mkdir(t, filepath.Join(dir, "runs")) // empty: no runs recorded yet
	f.cfg = filepath.Join(dir, "config.toml")
	write(t, f.cfg)

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, filepath.Join(dir, "runs"))
	mustExist(t, f.cfg)
}

func TestUninstallAllResolvesRelativeDotDotFromPhysicalCwd(t *testing.T) {
	f := newFixture(t)
	nested := filepath.Join(f.home, "srv/nested")
	mkdir(t, nested)
	link := filepath.Join(f.home, "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	real := filepath.Join(f.home, "srv/config.toml") // what the kernel opens for ../config.toml
	decoy := filepath.Join(f.home, "config.toml")    // what text-cleaning $PWD/../config.toml gives
	write(t, real)
	write(t, decoy)
	t.Chdir(link) // sets PWD to the logical path through the link
	f.cfg = "../config.toml"

	if code, _, stderr := f.uninstall(t, "", "--all", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, real)
	mustExist(t, decoy)
}

func TestUninstallAllKeepsSymlinkedConfigFolder(t *testing.T) {
	f := newFixture(t)
	// ~/.config/second-opinion -> dotfiles/second-opinion, holding only the config.
	target := filepath.Join(t.TempDir(), "dotfiles/second-opinion")
	mkdir(t, target)
	write(t, filepath.Join(target, "config.toml"))
	cfgDir := filepath.Dir(f.cfg)
	os.RemoveAll(cfgDir)
	if err := os.Symlink(target, cfgDir); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	if code, _, stderr := f.uninstall(t, "", "--all", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, filepath.Join(target, "config.toml"))
	if _, err := os.Stat(cfgDir); err != nil {
		t.Errorf("the symlinked config folder should still resolve (no dangling link): %v", err)
	}
}

func TestUninstallAllKeepsSymlinkedConfigFolderWhateverTheSpelling(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spell func(f fixture, cfgDir string) string // how the config path is written
		chdir bool                                  // run from inside the linked folder
		sub   bool                                  // the spelling needs a subfolder to exist
	}{
		{"relative from inside the link", func(f fixture, cfgDir string) string { return "config.toml" }, true, false},
		{"trailing dot", func(f fixture, cfgDir string) string { return cfgDir + "/./config.toml" }, false, false},
		// For "sub/.." to resolve, sub must exist, so the folder can never end
		// up empty; this case only checks nothing breaks.
		{"trailing dot-dot", func(f fixture, cfgDir string) string { return cfgDir + "/sub/../config.toml" }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			target := filepath.Join(t.TempDir(), "dotfiles/second-opinion")
			mkdir(t, target)
			if tc.sub {
				mkdir(t, filepath.Join(target, "sub"))
			}
			write(t, filepath.Join(target, "config.toml"))
			cfgDir := filepath.Dir(f.cfg)
			os.RemoveAll(cfgDir)
			if err := os.Symlink(target, cfgDir); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
			if tc.chdir {
				t.Chdir(cfgDir)
			}
			f.cfg = tc.spell(f, cfgDir)

			if code, _, stderr := f.uninstall(t, "", "--all", "--yes"); code != 0 {
				t.Fatalf("exit code = %d: %s", code, stderr)
			}
			mustBeGone(t, filepath.Join(target, "config.toml"))
			if _, err := os.Stat(cfgDir); err != nil {
				t.Errorf("the symlinked config folder should still resolve (no dangling link): %v", err)
			}
		})
	}
}

func TestResolveConfigPathRootedWithoutDrive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("rooted paths without a drive only exist on Windows")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := resolveConfigPath(`\config.toml`)
	want := filepath.VolumeName(wd) + `\config.toml`
	if err != nil || !strings.EqualFold(got, want) {
		t.Errorf("resolveConfigPath = %q, %v; want %q, the file Windows opens", got, err, want)
	}
}

// dotDotLayout builds home/link -> home/srv/nested and home/srv/so -> shared/so,
// so that home/link/../so is, to the filesystem, home/srv/so (a link to the
// running binary), while text-cleaning gives home/so.
func dotDotLayout(t *testing.T, f fixture) (soLink, target string) {
	t.Helper()
	nested := filepath.Join(f.home, "srv/nested")
	mkdir(t, nested)
	if err := os.Symlink(nested, filepath.Join(f.home, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	target = filepath.Join(t.TempDir(), "shared/so")
	mkdir(t, filepath.Dir(target))
	write(t, target)
	os.Chmod(target, 0o755)
	soLink = filepath.Join(f.home, "srv/so")
	if err := os.Symlink(target, soLink); err != nil {
		t.Fatal(err)
	}
	executable = func() (string, error) { return target, nil }
	return soLink, target
}

func TestUninstallBinaryStartedThroughDotDot(t *testing.T) {
	f := newFixture(t)
	soLink, target := dotDotLayout(t, f)
	invokedAs = func() string { return filepath.Join(f.home, "link") + "/../so" }

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, soLink)
	mustExist(t, target)
}

func TestUninstallBinaryFoundThroughDotDotPathEntry(t *testing.T) {
	f := newFixture(t)
	soLink, target := dotDotLayout(t, f)
	t.Setenv("PATH", filepath.Join(f.home, "link")+"/..")
	invokedAs = func() string { return "so" }

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, soLink)
	mustExist(t, target)
}

func TestUninstallKeepsBinaryWhenStartUnknown(t *testing.T) {
	f := newFixture(t)
	invokedAs = func() string { return "so" }
	t.Setenv("PATH", t.TempDir()) // so cannot be found again

	code, out, _ := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	mustExist(t, f.bin)
	if !strings.Contains(out, "could not tell how so was started") {
		t.Errorf("output should explain why the binary was kept:\n%s", out)
	}
}

func TestUninstallAllKeepsSymlinkedConfigFolderSharedWithHistory(t *testing.T) {
	f := newFixture(t)
	// The config folder is a symlink to the state folder itself.
	cfgDir := filepath.Dir(f.cfg)
	os.RemoveAll(cfgDir)
	if err := os.Symlink(f.state, cfgDir); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	write(t, filepath.Join(f.state, "config.toml"))

	if code, _, stderr := f.uninstall(t, "", "--all", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	if _, err := os.Stat(cfgDir); err != nil {
		t.Errorf("the symlinked config folder should still resolve (no dangling link): %v", err)
	}
}

func TestResolveConfigPathFlagsUnclearFolders(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "target")
	mkdir(t, filepath.Join(target, "sub"))
	link := filepath.Join(home, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	plain := filepath.Join(home, "plain")
	mkdir(t, plain)
	t.Chdir(plain)

	for raw, want := range map[string]bool{
		filepath.Join(plain, "config.toml"): false, // a plain absolute path is clear
		"config.toml":                       true,  // relative
		link + "/config.toml":               true,  // the folder is a symlink
		link + "/./config.toml":             true,  // "."
		link + "/sub/../config.toml":        true,  // ".."
	} {
		if _, got, err := resolveConfigPath(raw); err != nil || got != want {
			t.Errorf("resolveConfigPath(%q) parentIsLink = %v, %v; want %v", raw, got, err, want)
		}
	}
}

func TestUninstallKeepsBinaryStartedFromDifferentFile(t *testing.T) {
	f := newFixture(t)
	// argv[0] names a different file called so than the one actually running.
	other := filepath.Join(t.TempDir(), "so")
	write(t, other)
	os.Chmod(other, 0o755)
	invokedAs = func() string { return other }

	code, out, _ := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	mustExist(t, other, f.bin)
	if !strings.Contains(out, "which is a different file") {
		t.Errorf("output should explain why the binary was kept:\n%s", out)
	}
	if strings.Contains(out, "another so is still on your PATH") {
		t.Errorf("the kept running binary must not be reported as another so:\n%s", out)
	}
}

func TestUninstallKeepsLinkToDifferentBinary(t *testing.T) {
	f := newFixture(t)
	// so was started through a link named so that points to a different executable.
	other := filepath.Join(t.TempDir(), "other-tool")
	write(t, other)
	os.Chmod(other, 0o755)
	link := filepath.Join(t.TempDir(), "so")
	if err := os.Symlink(other, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	invokedAs = func() string { return link }

	code, out, _ := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	mustExist(t, link, other, f.bin)
	if !strings.Contains(out, "which does not point to it") {
		t.Errorf("output should explain why the binary was kept:\n%s", out)
	}
}

func TestUninstallKeepsSkillFolderAnotherHarnessLinksTo(t *testing.T) {
	f := newFixture(t)
	// ~/.codex/skills/second-opinion -> ~/.claude/skills/second-opinion
	claude := filepath.Join(f.home, ".claude/skills/second-opinion")
	codex := filepath.Join(f.home, ".codex/skills/second-opinion")
	mkdir(t, filepath.Dir(codex))
	if err := os.Symlink(claude, codex); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	code, out, stderr := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, filepath.Join(claude, "SKILL.md"))
	if _, err := os.Stat(codex); err != nil {
		t.Errorf("the codex skill link should still resolve (no dangling link): %v", err)
	}
	if !strings.Contains(out, "kept the folder because a symlink may lead to it") {
		t.Errorf("output should explain why the folder was kept:\n%s", out)
	}
}

func TestUninstallAllKeepsFolderSymlinkedRunsPointsTo(t *testing.T) {
	f := newFixture(t)
	// runs -> the config folder, which holds only config.toml.
	runs := filepath.Join(f.state, "runs")
	os.RemoveAll(runs)
	cfgDir := filepath.Dir(f.cfg)
	if err := os.Symlink(cfgDir, runs); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if code, _, stderr := f.uninstall(t, "", "--all", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, f.cfg)
	if _, err := os.Stat(runs); err != nil {
		t.Errorf("the runs link should still resolve (no dangling link): %v", err)
	}
}

func TestUninstallOnWindowsRemovesOnlyTheLink(t *testing.T) {
	f := newFixture(t)
	goos = "windows"
	target := filepath.Join(t.TempDir(), "apps/so.exe")
	mkdir(t, filepath.Dir(target))
	write(t, target)
	link := filepath.Join(f.home, "bin/so.exe")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	executable = func() (string, error) { return link, nil } // Windows reports the link

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, link)
	mustExist(t, target)
	if exists(target + ".old") {
		t.Error("the link's target should not be moved aside")
	}
}

func TestUninstallNotesHardlinkedSoLeftOnPath(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(t.TempDir(), "other-bin")
	mkdir(t, other)
	if err := os.Link(f.bin, filepath.Join(other, "so")); err != nil {
		t.Skip("hard links unavailable:", err)
	}
	t.Setenv("PATH", filepath.Join(f.home, "bin")+string(os.PathListSeparator)+other)

	_, out, _ := f.uninstall(t, "", "--yes")
	mustBeGone(t, f.bin)
	if !strings.Contains(out, "another so is still on your PATH at "+filepath.Join(other, "so")) {
		t.Errorf("a working so left on PATH must be reported:\n%s", out)
	}
}

func TestUninstallRemovesPathLinksToTheBinary(t *testing.T) {
	f := newFixture(t)
	// ~/bin2/so -> ~/bin/so; so is run directly as ~/bin/so.
	bin2 := filepath.Join(f.home, "bin2")
	mkdir(t, bin2)
	link := filepath.Join(bin2, "so")
	if err := os.Symlink(f.bin, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	t.Setenv("PATH", filepath.Join(f.home, "bin")+string(os.PathListSeparator)+bin2)

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, f.bin, link)
}

func TestUninstallKeepsPathLinkToHardlinkOfBinary(t *testing.T) {
	f := newFixture(t)
	// /opt/so is a hard link to the binary (kept), and bin2/so links to it.
	opt := filepath.Join(f.home, "opt")
	mkdir(t, opt)
	hard := filepath.Join(opt, "so")
	if err := os.Link(f.bin, hard); err != nil {
		t.Skip("hard links unavailable:", err)
	}
	bin2 := filepath.Join(f.home, "bin2")
	mkdir(t, bin2)
	link := filepath.Join(bin2, "so")
	if err := os.Symlink(hard, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	t.Setenv("PATH", filepath.Join(f.home, "bin")+string(os.PathListSeparator)+bin2)

	if code, _, stderr := f.uninstall(t, "", "--yes"); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	mustBeGone(t, f.bin)
	mustExist(t, hard, link)
}

func TestUninstallKeepsPathLinkToDifferentExecutable(t *testing.T) {
	f := newFixture(t)
	tool := filepath.Join(t.TempDir(), "other-so")
	write(t, tool)
	os.Chmod(tool, 0o755)
	bin2 := filepath.Join(f.home, "bin2")
	mkdir(t, bin2)
	link := filepath.Join(bin2, "so")
	if err := os.Symlink(tool, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	t.Setenv("PATH", filepath.Join(f.home, "bin")+string(os.PathListSeparator)+bin2)

	_, out, _ := f.uninstall(t, "", "--yes")
	mustBeGone(t, f.bin)
	mustExist(t, link, tool)
	if !strings.Contains(out, "another so is still on your PATH at "+link) {
		t.Errorf("the other so should be reported:\n%s", out)
	}
}

func TestUninstallNotesKeptTargetOfRemovedLink(t *testing.T) {
	f := newFixture(t)
	// so is started through bin/so -> build/so, and build is on PATH too.
	build := filepath.Join(f.home, "build")
	mkdir(t, build)
	target := filepath.Join(build, "so")
	write(t, target)
	os.Chmod(target, 0o755)
	link := filepath.Join(f.home, "bin/so")
	os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	t.Setenv("PATH", filepath.Join(f.home, "bin")+string(os.PathListSeparator)+build)
	executable = func() (string, error) { return target, nil }
	invokedAs = func() string { return "so" }

	_, out, _ := f.uninstall(t, "", "--yes")
	mustBeGone(t, link)
	mustExist(t, target)
	if !strings.Contains(out, "so is still on your PATH at "+target+" (the binary the removed link pointed to)") {
		t.Errorf("the kept target on PATH should be reported:\n%s", out)
	}
}

// unsetEnv removes an environment variable for the test, restoring it after.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	if v, ok := os.LookupEnv(key); ok {
		t.Cleanup(func() { os.Setenv(key, v) })
	}
	os.Unsetenv(key)
}

func TestUninstallFindsSkillUnderMovedHarnessHome(t *testing.T) {
	f := newFixture(t)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_HOME", codexHome)
	moved := filepath.Join(codexHome, "skills", "second-opinion", "SKILL.md")
	mkdir(t, filepath.Dir(moved))
	write(t, moved)
	other := filepath.Join(codexHome, "skills", "other", "SKILL.md")
	mkdir(t, filepath.Dir(other))
	write(t, other)

	code, out, stderr := f.uninstall(t, "", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Errorf("the skill under CODEX_HOME survived uninstall (err %v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another skill under CODEX_HOME was removed: %v", err)
	}
}
