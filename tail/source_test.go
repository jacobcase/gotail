package tail_test

import (
	"context"
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
