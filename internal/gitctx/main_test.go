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
	os.Exit(m.Run())
}
