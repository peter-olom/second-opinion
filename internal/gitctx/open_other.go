//go:build !unix

package gitctx

import "os"

// openNoFollow opens path for reading. Without O_NOFOLLOW, the caller's
// Lstat-then-SameFile check is what rejects a symlink swapped in.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
