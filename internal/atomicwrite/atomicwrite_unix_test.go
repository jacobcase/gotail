//go:build unix

package atomicwrite_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcase/gotail/v2/internal/atomicwrite"
)

// TestWrite_RejectsSymlinkAtTmp: Write must not follow a pre-positioned
// symlink at path+".tmp". Currently Write uses O_TRUNC with no O_NOFOLLOW,
// so it opens through the symlink and overwrites the target. The fix adds
// O_NOFOLLOW|O_EXCL so Write errors on a pre-existing symlink.
func TestWrite_RejectsSymlinkAtTmp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")
	tmp := path + ".tmp"

	target := filepath.Join(dir, "innocent.dat")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatal(err)
	}

	err := atomicwrite.Write(path, []byte(`{"version":1}`), 0o600, false)
	if err == nil {
		t.Fatal("Write through pre-positioned symlink at .tmp: want error, got nil")
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("ReadFile target: %v", readErr)
	}
	if string(got) != "untouched" {
		t.Fatalf("symlink target was overwritten: got %q, want %q", got, "untouched")
	}
}
