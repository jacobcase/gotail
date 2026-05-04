package watch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcase/gotail/v2/watch"
)

// TestStatInode_NonZero asserts that a freshly created file produces a
// nonzero stable identity on every supported platform — including Windows,
// where the previous implementation always returned 0.
func TestStatInode_NonZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	if err := os.WriteFile(path, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := watch.StatInode(path)
	if err != nil {
		t.Fatalf("StatInode: %v", err)
	}
	if id == 0 {
		t.Fatalf("StatInode returned 0; expected a stable nonzero file identity")
	}
}

// TestStatInode_DiffersAfterRename pins the rotation invariant: after
// rename+create, the path resolves to a different file, so its identity
// must differ from the original. On platforms where StatInode returns 0
// (e.g., ReFS), callers are expected to use NoInodeCheck — but on standard
// dev/CI filesystems (ext4, APFS, NTFS) we want a real difference.
func TestStatInode_DiffersAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	backup := filepath.Join(dir, "app.log.1")

	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idOld, err := watch.StatInode(path)
	if err != nil {
		t.Fatalf("StatInode old: %v", err)
	}

	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	idNew, err := watch.StatInode(path)
	if err != nil {
		t.Fatalf("StatInode new: %v", err)
	}

	if idOld == 0 || idNew == 0 {
		t.Skipf("filesystem does not provide stable identity (old=%d new=%d); skipping", idOld, idNew)
	}
	if idOld == idNew {
		t.Fatalf("expected different identities after rename+create, got %d for both", idOld)
	}
}
