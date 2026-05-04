//go:build windows

package tail

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// errLockViolation is Windows error 33 (ERROR_LOCK_VIOLATION), returned by
// LockFileEx when the file is already locked and LOCKFILE_FAIL_IMMEDIATELY is set.
const errLockViolation syscall.Errno = 33

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

type flock struct{ f *os.File }

func acquireFlock(path string) (*flock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("tail: open lock file %s: %w", path, err)
	}

	h := syscall.Handle(f.Fd())
	var ol syscall.Overlapped
	r, _, e := procLockFileEx.Call(
		uintptr(h),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0, // reserved
		1, 0, // lock 1 byte at offset 0
		uintptr(unsafe.Pointer(&ol)),
	)
	if r == 0 {
		f.Close()
		if e == errLockViolation {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("tail: LockFileEx %s: %w", path, e)
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
	h := syscall.Handle(l.f.Fd())
	var ol syscall.Overlapped
	procUnlockFileEx.Call(uintptr(h), 0, 1, 0, uintptr(unsafe.Pointer(&ol))) //nolint:errcheck
	err := l.f.Close()
	l.f = nil
	return err
}
