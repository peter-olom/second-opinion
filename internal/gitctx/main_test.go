package gitctx

import (
	"os"
	"testing"
)

// TestMain keeps the developer's global and system git config (signing,
// excludes, filters, diff settings) out of the tests, for git run by the
// tests and by the code under test.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Run from a git hook, these would point the fixtures' git commands at
	// the developer's own repository and index.
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX",
		"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_TEMPLATE_DIR", "GIT_CEILING_DIRECTORIES"} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
