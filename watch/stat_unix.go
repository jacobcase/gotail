//go:build unix

package watch

import (
	"os"
	"syscall"
)

// fileID returns a stable file identity for an open file.
// On Unix this is the inode from fstat(2).
func fileID(f *os.File) uint64 {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}
