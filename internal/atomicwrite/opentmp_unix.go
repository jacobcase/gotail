//go:build unix

package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// openTmp opens (or creates) tmp without following symlinks. A pre-existing
// regular file is treated as a stale tmp from a crashed prior write and
// removed so the O_EXCL open can succeed. A pre-existing symlink (or other
// non-regular entry) is refused — silently removing it would defeat the
// purpose of O_NOFOLLOW, since an attacker could plant a symlink between
// removal and re-creation.
func openTmp(tmp string, mode os.FileMode) (*os.File, error) {
	if fi, err := os.Lstat(tmp); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing to follow symlink at tmp path %s", tmp)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("tmp path %s is not a regular file (mode=%v)", tmp, fi.Mode())
		}
		// Stale regular tmp from a crashed prior write; remove so the
		// O_EXCL open below can succeed.
		if err := os.Remove(tmp); err != nil {
			return nil, fmt.Errorf("remove stale tmp: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("lstat tmp: %w", err)
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC | os.O_EXCL | syscall.O_NOFOLLOW
	return os.OpenFile(tmp, flags, mode)
}
