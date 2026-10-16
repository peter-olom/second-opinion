//go:build !unix

package backend

import "os/exec"

// On Windows, cancellation kills only the CLI process itself (os/exec's
// default), not helpers it started.
func isolateProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) {}
