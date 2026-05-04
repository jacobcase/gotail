package tail

import "context"

// Source enumerates the files that form a logical log stream.
// Files are returned oldest-first; the last element is the active file.
// Enumerate must be stable across calls until a file has been fully consumed
// and pruned from disk.
type Source interface {
	Enumerate(ctx context.Context) ([]string, error)
}

// SingleFile returns a [Source] that always enumerates exactly one file.
func SingleFile(path string) Source {
	return &singleFileSource{path: path}
}

type singleFileSource struct{ path string }

func (s *singleFileSource) Enumerate(_ context.Context) ([]string, error) {
	return []string{s.path}, nil
}

// MemorySource returns a [Source] backed by a fixed, immutable slice of paths.
// Intended for tests; use tailtest.MemorySource for mutable mid-tail scenarios.
func MemorySource(paths []string) Source {
	cp := make([]string, len(paths))
	copy(cp, paths)
	return &memorySource{paths: cp}
}

type memorySource struct{ paths []string }

func (m *memorySource) Enumerate(_ context.Context) ([]string, error) {
	return m.paths, nil
}
