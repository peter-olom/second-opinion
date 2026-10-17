package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the history and config folders at a scratch directory for
// the whole package, so no test can read, record into, or prune the
// developer's real run history. Tests that need their own still set them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "so-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
