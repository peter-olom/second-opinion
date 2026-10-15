//go:build unix

package backend

import (
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel runs the CLI in its own process group and kills the
// whole group on cancellation, so helper processes the CLI spawned die with it.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
