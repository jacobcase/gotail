//go:build windows

package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// openTmp opens (or creates) tmp, refusing to operate through reparse points
// (symbolic links, junctions, mount points). On Windows, lstat returns a
// non-regular mode for any reparse point, so we reject it before calling
// OpenFile. A stale regular tmp is removed first.
func openTmp(tmp string, mode os.FileMode) (*os.File, error) {
	if fi, err := os.Lstat(tmp); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeIrregular != 0 {
			return nil, fmt.Errorf("tmp path %s is a reparse point or non-regular file; refusing to follow", tmp)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("tmp path %s is not a regular file (mode=%v)", tmp, fi.Mode())
		}
		if err := os.Remove(tmp); err != nil {
			return nil, fmt.Errorf("remove stale tmp: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("lstat tmp: %w", err)
	}

	return os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_EXCL, mode)
}
