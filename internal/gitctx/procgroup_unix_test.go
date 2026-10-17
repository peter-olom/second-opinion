//go:build unix

package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("helper %d kept running after git exited", pid)
	}
}

// alive reports whether pid is a running process. On Linux a killed orphan
// can linger as a zombie until it is reaped, which Signal(0) can't tell from
// a live one, so /proc decides there.
func alive(pid int) bool {
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		s := string(stat)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			return s[i+2] != 'Z' && s[i+2] != 'X'
		}
		return true
	}
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}
