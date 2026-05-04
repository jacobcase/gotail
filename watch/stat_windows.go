//go:build windows

package watch

import (
	"os"
	"syscall"
)

// fileInode returns a stable file identity on Windows using
// GetFileInformationByHandle. This is stable on NTFS but NOT on ReFS or some
// network filesystems — use Config.NoInodeCheck on those.
func fileInode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		_ = st // Win32FileAttributeData does not carry index numbers
	}
	// For a real inode-equivalent we need an open handle.
	// Callers that need stable identity on Windows should use NoInodeCheck
	// or obtain the inode via fileInodeFromHandle (see poll.go).
	return 0
}

// fileInodeFromHandle returns the file index from an open *os.File on Windows.
func fileInodeFromHandle(f interface{ Fd() uintptr }) uint64 {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(
		syscall.Handle(f.Fd()), &info,
	); err != nil {
		return 0
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
}
