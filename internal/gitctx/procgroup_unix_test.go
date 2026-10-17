//go:build unix

package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGitHelperDoesNotOutliveGit(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// A git that leaves a helper behind, as a hook or credential helper might.
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "pid")
	wrapper := "#!/bin/sh\ncase \"$*\" in *--src-prefix=a/*) sleep 60 >/dev/null 2>&1 & echo $! > " + pidFile + ";; esac\nexec " + realGit + " \"$@\"\n"
	os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := Diff(context.Background(), dir, "HEAD", ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the helper never started, so this test checks nothing: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid <= 0 {
		t.Fatalf("no helper PID recorded (%q)", data)
	}
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("helper %d kept running after git exited", pid)
	}
}
