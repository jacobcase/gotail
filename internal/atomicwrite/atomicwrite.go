// Package atomicwrite provides a write-to-tmp + fsync + rename helper so that
// cursor files are either fully old or fully new on disk after any crash.
package atomicwrite

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write writes data to path atomically:
//
//  1. Write data to path+".tmp" with mode.
//  2. fsync the temp file (data durability).
//  3. Close the temp file fd. Closing before rename is required on Windows
//     (rename-while-open can fail with sharing violation) and surfaces late
//     I/O errors that some filesystems (NFS, async mounts) only report at
//     close time after a successful fsync.
//  4. os.Rename(tmp, path).
//  5. If dirSync, fsync the containing directory (rename durability).
//
// Symlink safety: a pre-existing tmp at path+".tmp" must not redirect the
// open through a symlink. Unix builds add O_NOFOLLOW; Windows builds reject
// reparse points before opening. See openTmp_*.go.
func Write(path string, data []byte, mode os.FileMode, dirSync bool) error {
	tmp := path + ".tmp"
	f, err := openTmp(tmp, mode)
	if err != nil {
		return fmt.Errorf("atomicwrite: create %s: %w", tmp, err)
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("atomicwrite: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("atomicwrite: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("atomicwrite: close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("atomicwrite: rename: %w", err)
	}

	if dirSync {
		if d, err := os.Open(filepath.Dir(path)); err == nil {
			_ = d.Sync() // best-effort; some FSes (FAT32, SMB) don't support it
			d.Close()
		}
	}
	return nil
}
