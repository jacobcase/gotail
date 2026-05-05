//go:build unix

package watch

import (
	"os"
	"syscall"
)

// statSizeInode returns size + inode for path without holding an fd.
// On Unix this is a single os.Stat call; on Windows the inode requires
// an open handle (see stat_windows.go).
func statSizeInode(path string) (int64, uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fi.Size(), st.Ino, nil
	}
	return fi.Size(), 0, nil
}
