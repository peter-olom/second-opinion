package backend

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// nil0 is the "is it alive?" signal 0.
func nil0() os.Signal { return syscall.Signal(0) }

// alive reports whether pid is a running process. On Linux a killed process
// can linger as a zombie until it is reaped, which Signal(0) can't tell from
// a live one, so /proc decides there.
func alive(pid int) bool {
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		// The state follows the parenthesised command name.
		s := string(stat)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			return s[i+2] != 'Z' && s[i+2] != 'X'
		}
		return true
	}
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(nil0()) == nil
}
