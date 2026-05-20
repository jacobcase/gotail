// Package tailtest provides test helpers for the tail package.
//
// Construction idiom: helpers with required parameters expose a New*
// constructor; helpers with no required parameters (like [MemorySource])
// are zero-value-usable — declare with `var ms tailtest.MemorySource`
// and call methods directly. Mirrors the convention in [forwardtest]
// and [watchtest].
package tailtest

import (
	"context"
	"slices"
	"sync"
)

// MemorySource is a mutable, thread-safe [tail.Source] for controlled
// mid-tail rotation scenarios. Use [tail.StaticSource] for immutable test
// sources; use MemorySource when you need to Add or Prune files while a
// Tailer is running.
type MemorySource struct {
	mu    sync.Mutex
	paths []string
}

// Add appends path to the end of the file list.
func (m *MemorySource) Add(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paths = append(m.paths, path)
}

// Prune removes path from the file list. No-op if path is not present.
func (m *MemorySource) Prune(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, p := range m.paths {
		if p == path {
			m.paths = slices.Delete(m.paths, i, i+1)
			return
		}
	}
}

// Enumerate returns a snapshot of the current file list.
func (m *MemorySource) Enumerate(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]string, len(m.paths))
	copy(cp, m.paths)
	return cp, nil
}
