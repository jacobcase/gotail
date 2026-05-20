package atomicwrite_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jacobcase/gotail/v3/internal/atomicwrite"
)

func TestWrite_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dat")

	if err := atomicwrite.Write(path, []byte("hello"), 0o600, true); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}

	// No leftover tmp file.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file leaked: stat err = %v", err)
	}
}

func TestWrite_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dat")

	if err := atomicwrite.Write(path, []byte("first"), 0o600, false); err != nil {
		t.Fatalf("Write #1: %v", err)
	}
	// Overwriting an existing file exercises the rename-replace path,
	// which on Windows requires the source to be closed first.
	if err := atomicwrite.Write(path, []byte("second"), 0o600, false); err != nil {
		t.Fatalf("Write #2: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("got %q, want %q", got, "second")
	}
}

// TestWrite_RejectsDirectoryAtTmp verifies openTmp's "non-regular file"
// branch: if a directory is squatting at path+".tmp", Write must refuse
// rather than removing it (an attacker could otherwise plant a directory
// to redirect Write through path traversal on the Remove + recreate path).
func TestWrite_RejectsDirectoryAtTmp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dat")
	tmp := path + ".tmp"

	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := atomicwrite.Write(path, []byte("data"), 0o600, false); err == nil {
		t.Fatal("expected Write to fail when a directory squats at the tmp path")
	}

	fi, err := os.Stat(tmp)
	if err != nil {
		t.Fatalf("Stat tmp: %v", err)
	}
	if !fi.IsDir() {
		t.Fatal("squatting directory was removed by Write — must be left intact")
	}
}

// TestWrite_RenameFails_DestIsNonEmptyDir exercises the os.Rename failure
// branch: if the destination path is a non-empty directory, the rename
// fails after the tmp file was successfully written, and Write must clean
// up the orphaned tmp.
func TestWrite_RenameFails_DestIsNonEmptyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows MoveFileEx behaviour for replacing a directory with a
		// file is layered on the "replace existing" semantics the unix
		// rename(2) test relies on; skip rather than maintain divergent
		// expected error strings.
		t.Skip("rename-into-directory semantics differ on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "x.dat")

	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupant"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := atomicwrite.Write(path, []byte("data"), 0o600, false); err == nil {
		t.Fatal("expected Write to fail when destination is a non-empty directory")
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp leaked after rename failure: stat err = %v", err)
	}
}

func TestWrite_HonorsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not enforced the same way on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dat")

	if err := atomicwrite.Write(path, []byte("m"), 0o640, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 0640", got)
	}
}
