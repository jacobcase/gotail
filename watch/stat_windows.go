//go:build windows

package watch

import (
	"os"
	"syscall"
)

// fileID returns a stable file identity for an open file.
// On Windows this is FileIndexHigh<<32 | FileIndexLow from
// GetFileInformationByHandle. Stable on NTFS; not stable on ReFS or some
// network filesystems — use Config.NoInodeCheck on those.
func fileID(f *os.File) uint64 {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return 0
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
}

// statSizeInode returns size + inode for path. On Windows the file index
// requires an open handle, so this is open-stat-close.
func statSizeInode(path string) (int64, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	return fi.Size(), fileID(f), nil
}
