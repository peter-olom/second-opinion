//go:build unix

package gitctx

import (
	"os"
	"syscall"
)

// openNoFollow opens path for reading, failing if its last element is a symlink.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
