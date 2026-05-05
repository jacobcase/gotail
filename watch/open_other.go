//go:build !windows

package watch

import "os"

// openShared opens path for reading. On non-Windows platforms the share-mode
// dance does not apply: POSIX permits unlink/rename of an open file natively.
// See open_windows.go for why this exists.
func openShared(path string) (*os.File, error) {
	return os.Open(path)
}
