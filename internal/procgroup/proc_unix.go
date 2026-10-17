//go:build unix

package procgroup

import (
	"os/exec"
	"syscall"
)

// Isolate runs the command in its own process group, so it and any
// helpers it starts can be killed together, and kills the group on
// cancellation.
func Isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(cmd.SysProcAttr)
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Kill kills whatever is left of the command's process group after it
// exits. The group may already be gone; that is fine.
func Kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
