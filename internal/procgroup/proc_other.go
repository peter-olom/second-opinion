//go:build !unix

package procgroup

import "os/exec"

// On Windows, cancellation kills only the CLI process itself (os/exec's
// default), not helpers it started.
func Isolate(*exec.Cmd) {}

func Kill(*exec.Cmd) {}
