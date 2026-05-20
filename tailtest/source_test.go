package tailtest_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jacobcase/gotail/v3/tail"
	"github.com/jacobcase/gotail/v3/tailtest"
)

func TestMemorySource_AddEnumerate(t *testing.T) {
	t.Parallel()

	m := &tailtest.MemorySource{}
	m.Add("a")
	m.Add("b")
	m.Add("c")

	got, err := m.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("Enumerate = %v, want [a b c]", got)
	}
}

func TestMemorySource_Prune(t *testing.T) {
	t.Parallel()

	m := &tailtest.MemorySource{}
	m.Add("a")
	m.Add("b")
	m.Add("c")

	m.Prune("b")
	got, err := m.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("after Prune(b) = %v, want [a c]", got)
	}

	// Pruning a non-existent path is a documented no-op.
	m.Prune("nope")
	got, _ = m.Enumerate(context.Background())
	if !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("after Prune(nope) = %v, want [a c]", got)
	}

	// Prune all remaining entries.
	m.Prune("a")
	m.Prune("c")
	got, _ = m.Enumerate(context.Background())
	if len(got) != 0 {
		t.Fatalf("after pruning all: %v, want empty", got)
	}
}

func TestMemorySource_EnumerateReturnsSnapshot(t *testing.T) {
	t.Parallel()

	// Enumerate must return a copy — mutating its result must not affect
	// subsequent enumerations.
	m := &tailtest.MemorySource{}
	m.Add("a")
	m.Add("b")

	snap, err := m.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	snap[0] = "MUTATED"

	got, _ := m.Enumerate(context.Background())
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("MemorySource was aliased through Enumerate result: %v", got)
	}
}

func TestMemorySource_EnumerateRespectsContext(t *testing.T) {
	t.Parallel()

	m := &tailtest.MemorySource{}
	m.Add("a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.Enumerate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Enumerate err = %v, want context.Canceled", err)
	}
}

func TestMemorySource_ConcurrentMutation(t *testing.T) {
	t.Parallel()

	m := &tailtest.MemorySource{}
	const writers = 4
	const each = 200

	var wg sync.WaitGroup
	wg.Add(writers * 2)
	for w := range writers {
		go func() {
			defer wg.Done()
			for j := range each {
				m.Add(string(rune('a'+w)) + string(rune('0'+j%10)))
			}
		}()
		go func() {
			defer wg.Done()
			for range each {
				_, _ = m.Enumerate(context.Background())
			}
		}()
	}
	wg.Wait()
}

// Compile-time check: MemorySource must satisfy tail.Source.
var _ tail.Source = (*tailtest.MemorySource)(nil)
