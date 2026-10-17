//go:build unix

package gitctx

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFilterHelperDoesNotOutliveGit(t *testing.T) {
	dir := newRepo(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	write(t, dir, ".gitattributes", "*.enc filter=slow\n")
	write(t, dir, "a.enc", "one\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "enc")
	// Configured only now, so the setup's own git commands start no helper.
	run(t, dir, "config", "filter.slow.clean", "sh -c 'sleep 60 >/dev/null 2>&1 & echo $! > "+pidFile+"; cat'")
	write(t, dir, "a.enc", "two\n")

	if _, err := Diff(context.Background(), dir, "HEAD", ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the clean filter never ran, so this test checks nothing: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid <= 0 {
		t.Fatalf("no helper PID recorded (%q)", data)
	}
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("filter helper %d kept running after git exited", pid)
	}
}
