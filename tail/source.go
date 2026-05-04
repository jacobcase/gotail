package tail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

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

// Lumberjack returns a [Source] for log files rotated by
// [lumberjack v2]. It recognises backup files named
// <base>-YYYY-MM-DDTHH-MM-SS<ext> in the same directory as activePath,
// returns them oldest-first, and appends activePath last.
//
// Compressed (.gz) backups are not yet supported and are silently ignored.
//
// [lumberjack v2]: https://github.com/natefinish/lumberjack
func Lumberjack(activePath string) Source {
	return &lumberjackSource{activePath: activePath}
}

type lumberjackSource struct{ activePath string }

func (s *lumberjackSource) Enumerate(_ context.Context) ([]string, error) {
	dir := filepath.Dir(s.activePath)
	name := filepath.Base(s.activePath)
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)] // e.g. "events" from "events.log"
	prefix := stem + "-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("tail: lumberjack readdir %s: %w", dir, err)
	}

	type backup struct {
		path string
		ts   time.Time
	}
	var backups []backup

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		// Must start with "stem-" and end with ext (possibly "").
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		if ext != "" && !strings.HasSuffix(n, ext) {
			continue
		}
		// The timestamp portion between prefix and ext must be exactly 19 chars.
		inner := n[len(prefix) : len(n)-len(ext)]
		if len(inner) != 19 {
			continue
		}
		ts, err := time.Parse("2006-01-02T15-04-05", inner)
		if err != nil {
			continue
		}
		p := filepath.Join(dir, n)
		if p == s.activePath {
			continue // exclude the active file itself
		}
		backups = append(backups, backup{path: p, ts: ts})
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].ts.Before(backups[j].ts)
	})

	result := make([]string, 0, len(backups)+1)
	for _, b := range backups {
		result = append(result, b.path)
	}
	return append(result, s.activePath), nil
}

// Glob returns a [Source] for log files with an explicit backup glob pattern.
// backupGlob is passed to [filepath.Glob]; matches are sorted lexicographically
// (oldest-first for numeric suffixes), and activePath is appended last.
//
// Example for logrotate numeric suffixes:
//
//	tail.Glob("/var/log/app.log", "/var/log/app.log.[0-9]*")
func Glob(activePath, backupGlob string) Source {
	return &globSource{activePath: activePath, backupGlob: backupGlob}
}

type globSource struct {
	activePath string
	backupGlob string
}

func (g *globSource) Enumerate(_ context.Context) ([]string, error) {
	matches, err := filepath.Glob(g.backupGlob)
	if err != nil {
		return nil, fmt.Errorf("tail: glob %q: %w", g.backupGlob, err)
	}
	sort.Strings(matches)
	return append(matches, g.activePath), nil
}
