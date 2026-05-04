package atomicwrite_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jacobcase/gotail/v2/internal/atomicwrite"
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
