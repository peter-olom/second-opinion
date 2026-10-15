//go:build !unix

package backend

import "os/exec"

func killProcessGroupOnCancel(*exec.Cmd) {}
