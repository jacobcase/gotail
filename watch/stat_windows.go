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
