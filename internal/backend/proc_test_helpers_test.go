package backend

import (
	"os"
	"strconv"
	"syscall"
)

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// nil0 is the "is it alive?" signal 0.
func nil0() os.Signal { return syscall.Signal(0) }
