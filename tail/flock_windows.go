//go:build windows

package tail

import (
	"errors"
	"fmt"
	"io/fs"
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

// lockSentinelOffset is the byte offset LockFileEx locks for mutual
// exclusion. We deliberately pick a value far past any plausible lock-file
// size so the lock does not collide with the small PID payload at offset 0:
// LockFileEx is mandatory on Windows, so a lock at offset 0 would block
// other processes (and tests) from reading the PID via os.ReadFile. Locks
// past EOF are valid on NTFS and create no on-disk side effects.
const lockSentinelOffset = 0x7FFFFFFFFFFFFFFE

func sentinelOverlapped() syscall.Overlapped {
	var ol syscall.Overlapped
	ol.Offset = uint32(lockSentinelOffset & 0xFFFFFFFF)
	ol.OffsetHigh = uint32(lockSentinelOffset >> 32)
	return ol
}

type flock struct{ f *os.File }

func acquireFlock(path string) (*flock, error) {
	// Refuse reparse points (symlinks, junctions, mount points) so an
	// attacker can't redirect the lock through a planted reparse point.
	// Mirrors the §5.4 step 1 contract for the Unix path's O_NOFOLLOW.
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeIrregular != 0 {
			return nil, fmt.Errorf("tail: lock path %s is a reparse point or non-regular file; refusing to follow", path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("tail: lstat lock file %s: %w", path, err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("tail: open lock file %s: %w", path, err)
	}

	h := syscall.Handle(f.Fd())
	ol := sentinelOverlapped()
	r, _, e := procLockFileEx.Call(
		uintptr(h),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,    // reserved
		1, 0, // lock 1 byte at the sentinel offset (set via ol)
		uintptr(unsafe.Pointer(&ol)),
	)
	if r == 0 {
		f.Close()
		if errors.Is(e, errLockViolation) {
			return nil, fmt.Errorf("tail: lock held on %s: %w", path, ErrLockHeld)
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
	ol := sentinelOverlapped()
	procUnlockFileEx.Call(uintptr(h), 0, 1, 0, uintptr(unsafe.Pointer(&ol))) //nolint:errcheck
	err := l.f.Close()
	l.f = nil
	return err
}
