//go:build unix

package backend

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup runs the CLI in its own process group, so it and any
// helpers it starts can be killed together, and kills the group on
// cancellation.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(cmd.SysProcAttr)
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killProcessGroup kills whatever is left of the CLI's process group after it
// exits. The group may already be gone; that is fine.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
