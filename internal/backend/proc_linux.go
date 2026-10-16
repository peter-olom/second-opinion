//go:build linux

package backend

import "syscall"

// setParentDeathSignal makes Linux kill the CLI if so itself dies (for example
// SIGKILL from a harness), so panelists do not keep running and spending quota.
func setParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}
