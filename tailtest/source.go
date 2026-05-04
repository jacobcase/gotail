// Package tailtest provides test helpers for the tail package.
package tailtest

import (
	"context"
	"slices"
	"sync"
)

// MemorySource is a mutable, thread-safe [tail.Source] for controlled
// mid-tail rotation scenarios. Use [tail.MemorySource] for immutable test
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
func (m *MemorySource) Enumerate(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]string, len(m.paths))
	copy(cp, m.paths)
	return cp, nil
}
