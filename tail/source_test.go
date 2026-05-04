package tail_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcase/gotail/v2/tail"
)

func TestSingleFileSource(t *testing.T) {
	src := tail.SingleFile("/var/log/app.log")
	paths, err := src.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/var/log/app.log" {
		t.Fatalf("want [/var/log/app.log], got %v", paths)
	}
}

func TestMemorySource(t *testing.T) {
	in := []string{"a.log", "b.log", "c.log"}
	src := tail.MemorySource(in)
	paths, err := src.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(paths) != len(in) {
		t.Fatalf("want %d paths, got %d", len(in), len(paths))
	}
	for i, p := range paths {
		if p != in[i] {
			t.Fatalf("paths[%d]: want %q, got %q", i, in[i], p)
		}
	}

	// Mutating the original slice must not affect the source.
	in[0] = "mutated"
	paths2, _ := src.Enumerate(context.Background())
	if paths2[0] == "mutated" {
		t.Fatal("MemorySource did not copy its input slice")
	}
}

func TestLumberjackSource_OrderedEnumeration(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "events.log")

	// Create files out of order to verify sort.
	backups := []string{
		"events-2024-03-01T10-00-00.log",
		"events-2024-01-01T00-00-00.log", // oldest
		"events-2024-02-15T12-30-00.log",
	}
	for _, b := range backups {
		touch(t, filepath.Join(dir, b))
	}
	touch(t, active)

	src := tail.Lumberjack(active)
	paths, err := src.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}

	if len(paths) != 4 {
		t.Fatalf("want 4 paths, got %d: %v", len(paths), paths)
	}

	// Verify active is last.
	if paths[len(paths)-1] != active {
		t.Fatalf("active must be last, got %q", paths[len(paths)-1])
	}

	// Verify backups are oldest-first.
	wantOrder := []string{
		"events-2024-01-01T00-00-00.log",
		"events-2024-02-15T12-30-00.log",
		"events-2024-03-01T10-00-00.log",
	}
	for i, want := range wantOrder {
		if filepath.Base(paths[i]) != want {
			t.Fatalf("paths[%d]: want %q, got %q", i, want, filepath.Base(paths[i]))
		}
	}
}

func TestLumberjackSource_NamingEdgeCases(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		active   string
		extra    []string // files in same dir
		wantLen  int      // total paths including active
	}{
		{
			// No extension active file.
			active:  filepath.Join(dir, "mylog"),
			extra:   []string{"mylog-2024-01-01T00-00-00", "mylog-not-a-timestamp", "mylog.other"},
			wantLen: 2, // valid backup + active
		},
		{
			// Multiple dots in name.
			active:  filepath.Join(dir, "app.json.log"),
			extra:   []string{"app.json-2024-01-01T00-00-00.log", "app.json-not-ts.log", "other.log"},
			wantLen: 2, // valid backup + active
		},
		{
			// Non-matching siblings must be excluded.
			active:  filepath.Join(dir, "svc.log"),
			extra:   []string{"svc-notts.log", "othersvc-2024-01-01T00-00-00.log", "svc-2024-01-01.log"},
			wantLen: 1, // only active (none of the backups match the strict pattern)
		},
	}

	for _, tt := range tests {
		t.Run(filepath.Base(tt.active), func(t *testing.T) {
			touch(t, tt.active)
			for _, e := range tt.extra {
				touch(t, filepath.Join(dir, e))
			}

			src := tail.Lumberjack(tt.active)
			paths, err := src.Enumerate(context.Background())
			if err != nil {
				t.Fatalf("Enumerate: %v", err)
			}
			if len(paths) != tt.wantLen {
				t.Fatalf("want %d paths, got %d: %v", tt.wantLen, len(paths), paths)
			}
			if paths[len(paths)-1] != tt.active {
				t.Fatalf("active must be last, got %q", paths[len(paths)-1])
			}
		})
	}
}

func TestGlobSource_Patterns(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")

	for _, name := range []string{"app.log.1", "app.log.3", "app.log.2"} {
		touch(t, filepath.Join(dir, name))
	}
	touch(t, active)

	src := tail.Glob(active, filepath.Join(dir, "app.log.[0-9]"))
	paths, err := src.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}

	if len(paths) != 4 {
		t.Fatalf("want 4 paths, got %d: %v", len(paths), paths)
	}
	if paths[len(paths)-1] != active {
		t.Fatalf("active must be last, got %q", paths[len(paths)-1])
	}

	// Lexicographic sort: .1 < .2 < .3
	for i, suffix := range []string{"1", "2", "3"} {
		want := filepath.Join(dir, "app.log."+suffix)
		if paths[i] != want {
			t.Fatalf("paths[%d]: want %q, got %q", i, want, paths[i])
		}
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
