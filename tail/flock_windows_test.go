//go:build windows

package tail_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jacobcase/gotail/v2/tail"
)

func testFlockConflict(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")
	cursorPath := filepath.Join(dir, "test.cursor")

	c1, err := tail.NewFileCursor(cursorPath, tail.WithFlock(lockPath))
	if err != nil {
		t.Fatalf("first cursor: %v", err)
	}
	defer c1.Close()

	c2Path := filepath.Join(dir, "test2.cursor")
	_, err = tail.NewFileCursor(c2Path, tail.WithFlock(lockPath))
	if !errors.Is(err, tail.ErrLockHeld) {
		t.Fatalf("want ErrLockHeld, got %v", err)
	}
}

func testFlockReleasedOnClose(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "rel.lock")
	cursorPath := filepath.Join(dir, "rel.cursor")

	c1, err := tail.NewFileCursor(cursorPath, tail.WithFlock(lockPath))
	if err != nil {
		t.Fatalf("first cursor: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close c1: %v", err)
	}

	c2, err := tail.NewFileCursor(cursorPath, tail.WithFlock(lockPath))
	if err != nil {
		t.Fatalf("second cursor after release: %v", err)
	}
	c2.Close()
}

func testFlockCrossProcess(t *testing.T) {
	t.Skip("cross-process flock test not yet ported to Windows (LockFileEx semantics differ from POSIX flock)")
}

func testFlockSymlinkFollow(t *testing.T) {
	t.Skip("symlink-follow flock test not applicable on Windows (reparse points differ from POSIX symlinks)")
}

func testFlockPIDInFile(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "pid.lock")
	cursorPath := filepath.Join(dir, "pid.cursor")

	c, err := tail.NewFileCursor(cursorPath, tail.WithFlock(lockPath))
	if err != nil {
		t.Fatalf("NewFileCursor: %v", err)
	}
	defer c.Close()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("lock file does not contain a PID, got %q: %v", pidStr, err)
	}
	if pid != os.Getpid() {
		t.Fatalf("lock file PID %d != current PID %d", pid, os.Getpid())
	}
}
