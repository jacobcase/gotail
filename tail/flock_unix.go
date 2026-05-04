//go:build unix

package tail

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

type flock struct{ f *os.File }

func acquireFlock(path string) (*flock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("tail: open lock file %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
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
