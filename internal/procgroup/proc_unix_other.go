//go:build unix && !linux

package procgroup

import "syscall"

// setParentDeathSignal is Linux-only. On macOS and the BSDs, so kills the
// process group on cancellation and after every run, and catches SIGHUP, but a
// SIGKILL of so itself can still leave a panelist running.
func setParentDeathSignal(*syscall.SysProcAttr) {}
