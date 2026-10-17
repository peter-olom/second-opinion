package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the home, history and config folders at a scratch directory
// for the whole package, so no test can read, record into, or prune the
// developer's real run history, or install into their real skill folders.
// Tests that need their own still set them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "so-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	// Skill installs and pi's config are found under the home folder.
	if err := os.Mkdir(filepath.Join(dir, "home"), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", filepath.Join(dir, "home"))
	os.Setenv("USERPROFILE", filepath.Join(dir, "home"))
	for _, k := range []string{"PI_CODING_AGENT_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "SECOND_OPINION_CONFIG"} {
		os.Unsetenv(k)
	}
	// The fixtures run git too: keep the developer's git config, and a git
	// hook's routing variables (which could point at their real index), out.
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX",
		"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_TEMPLATE_DIR", "GIT_CEILING_DIRECTORIES"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
