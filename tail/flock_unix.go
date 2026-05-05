//go:build unix

package tail

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

type flock struct{ f *os.File }

func acquireFlock(path string) (*flock, error) {
	// Refuse to follow a pre-positioned symlink at lockPath. Without this
	// guard, an attacker who can plant a symlink in the lock-file's parent
	// directory can redirect the flock open to any file the gotail process
	// can write — and the subsequent Truncate(0)+PID write zeroes that
	// target. O_NOFOLLOW makes the open fail with ELOOP on a symlink.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("tail: open lock file %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("tail: flock %s: %w", path, err)
	}

	// Write holder PID (best-effort; not load-bearing).
	_ = f.Truncate(0)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")

	return &flock{f: f}, nil
}

func (l *flock) release() error {
	if l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}
