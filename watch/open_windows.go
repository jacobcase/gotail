//go:build windows

package watch

import (
	"os"
	"syscall"
)

// openShared opens path for reading with a share mode that allows other
// processes (including the same process) to rename or delete the file while
// this fd is open. Go's stdlib os.Open on Windows omits FILE_SHARE_DELETE
// from the share mode (see syscall_windows.go: sharemode = FILE_SHARE_READ |
// FILE_SHARE_WRITE), which blocks the rotation pattern that Linux/macOS allow
// natively. Without this helper, an external rename of the tailed file fails
// with ERROR_SHARING_VIOLATION while the LineReader holds its fd open.
//
// The returned *os.File has the same finalizer/close semantics as os.Open's
// result; callers do not need to special-case Windows.
func openShared(path string) (*os.File, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(
		pathp,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, // default security attributes
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0, // no template handle
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
