//go:build unix

package tail_test

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

	// Second cursor on the same lock must fail.
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

	// Lock must be available now.
	c2, err := tail.NewFileCursor(cursorPath, tail.WithFlock(lockPath))
	if err != nil {
		t.Fatalf("second cursor after release: %v", err)
	}
	c2.Close()
}

// helperEnvLock is the env var the parent sets to put the re-execed test
// binary into "helper" mode for testFlockCrossProcess.
const helperEnvLock = "GOTAIL_FLOCK_HELPER_LOCK"

// TestFlockHelperProcess is the helper-mode entry point for the cross-process
// flock test. It is a no-op during normal `go test` runs; only the parent
// test re-execs the binary with helperEnvLock set, which switches it into
// helper mode. In helper mode it acquires the cursor flock, signals readiness
// to stdout, then holds the lock until its stdin closes.
func TestFlockHelperProcess(t *testing.T) {
	lockPath := os.Getenv(helperEnvLock)
	if lockPath == "" {
		return // not in helper mode
	}

	c, err := tail.NewFileCursor(lockPath+".cursor", tail.WithFlock(lockPath))
	if err != nil {
		// Surface failure to the parent via exit code; the parent reads
		// stderr on a non-zero exit and reports it.
		_, _ = os.Stderr.WriteString("helper: NewFileCursor: " + err.Error() + "\n")
		os.Exit(2)
	}

	// Signal readiness, then block on stdin so the parent controls exit.
	if _, werr := os.Stdout.WriteString("READY\n"); werr != nil {
		_ = c.Close()
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = c.Close()

	// Bypass the test framework's PASS line so the parent's stdout pipe
	// only ever contains "READY\n".
	os.Exit(0)
}

// testFlockCrossProcess verifies that flock(2) is enforced across processes.
// flock_unix_test.go's other tests only exercise within-process conflict,
// which on Linux/macOS is per-fd-table and re-entrant from the same process.
// Forking the test binary gives a fresh fd-table whose advisory lock the
// parent must observe.
func testFlockCrossProcess(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "xproc.lock")

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestFlockHelperProcess$",
		"-test.timeout=30s",
	)
	cmd.Env = append(os.Environ(), helperEnvLock+"="+lockPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("helper Start: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	// Wait for the helper to acquire the lock and announce readiness.
	type readyResult struct {
		line string
		err  error
	}
	readyCh := make(chan readyResult, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		readyCh <- readyResult{line, err}
	}()

	select {
	case r := <-readyCh:
		if r.err != nil {
			errOut, _ := io.ReadAll(stderr)
			t.Fatalf("helper readiness read: %v; stderr=%q", r.err, errOut)
		}
		if strings.TrimSpace(r.line) != "READY" {
			t.Fatalf("helper sent unexpected readiness line: %q", r.line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not signal readiness within 5s")
	}

	// Helper now holds the lock in a separate fd table. Parent acquire must
	// observe the conflict — proving cross-process flock semantics, not
	// just same-process bookkeeping.
	cursorPath := filepath.Join(dir, "xproc-parent.cursor")
	_, err = tail.NewFileCursor(cursorPath, tail.WithFlock(lockPath))
	if !errors.Is(err, tail.ErrLockHeld) {
		t.Fatalf("parent acquire while helper holds lock: want ErrLockHeld, got %v", err)
	}
}

// testFlockSymlinkFollow: acquireFlock must not follow a pre-positioned
// symlink at lockPath. Currently os.OpenFile uses no O_NOFOLLOW, so it opens
// through the symlink and truncates the target. The fix adds O_NOFOLLOW.
func testFlockSymlinkFollow(t *testing.T) {
	t.Helper()
	dir := t.TempDir()

	target := filepath.Join(dir, "innocent.dat")
	if err := os.WriteFile(target, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(dir, "cursor.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}

	cursorPath := filepath.Join(dir, "cursor.json")
	c, err := tail.NewFileCursor(cursorPath, tail.WithFlock(lockPath))
	if err == nil {
		c.Close()
		t.Fatal("WithFlock through pre-positioned symlink: want error, got nil")
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("ReadFile target: %v", readErr)
	}
	if string(got) != "untouched" {
		t.Fatalf("symlink target was truncated/written: got %q", got)
	}
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
