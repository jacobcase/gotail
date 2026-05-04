package tail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// LumberjackOption is a functional option for [Lumberjack].
type LumberjackOption func(*lumberjackSource)

// WithLumberjackSkippedHook installs a callback that fires once per backup
// file Lumberjack recognises but cannot expose to the tailer. The hook is
// invoked synchronously from [Source.Enumerate]; it must not block.
//
// reason is a short tag identifying why the file was skipped:
//   - "compressed": the file is a recognised lumberjack-with-compression
//     backup (<stem>-<timestamp><ext>.gz). gotail does not decompress on
//     read; the hook lets callers log, alert, or treat checkpoint resume
//     against an aged-off backup as a hard error.
func WithLumberjackSkippedHook(fn func(path, reason string)) LumberjackOption {
	return func(s *lumberjackSource) { s.skipped = fn }
}

// Lumberjack returns a [Source] for log files rotated by
// [lumberjack v2]. It recognises backup files named
// <base>-YYYY-MM-DDTHH-MM-SS<ext> in the same directory as activePath,
// returns them oldest-first, and appends activePath last.
//
// Compressed (.gz) backups are not enumerated. Use
// [WithLumberjackSkippedHook] to observe them — for example, to alert
// when a checkpoint resume would target a backup that has already been
// compressed and aged out of reach.
//
// [lumberjack v2]: https://github.com/natefinish/lumberjack
func Lumberjack(activePath string, opts ...LumberjackOption) Source {
	s := &lumberjackSource{activePath: activePath}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type lumberjackSource struct {
	activePath string
	skipped    func(path, reason string) // optional
}

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
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		// Detect the .gz variant first so we can report it before the
		// regular suffix check excludes it as "not matching ext".
		if matchLumberjackCompressed(n, prefix, ext) {
			if s.skipped != nil {
				s.skipped(filepath.Join(dir, n), "compressed")
			}
			continue
		}
		if ext != "" && !strings.HasSuffix(n, ext) {
			continue
		}
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

// matchLumberjackCompressed reports whether n is the .gz form of a
// lumberjack backup (<prefix><19-char-ts><ext>.gz).
func matchLumberjackCompressed(n, prefix, ext string) bool {
	const gz = ".gz"
	if !strings.HasSuffix(n, gz) {
		return false
	}
	body := n[:len(n)-len(gz)]
	if ext != "" && !strings.HasSuffix(body, ext) {
		return false
	}
	if len(body) < len(prefix)+19+len(ext) {
		return false
	}
	inner := body[len(prefix) : len(body)-len(ext)]
	if len(inner) != 19 {
		return false
	}
	if _, err := time.Parse("2006-01-02T15-04-05", inner); err != nil {
		return false
	}
	return true
}

// Glob returns a [Source] for log files with an explicit backup glob pattern.
// backupGlob is passed to [filepath.Glob]; matches are sorted lexicographically
// and activePath is appended last.
//
// Lexicographic order is not the same as age order for numeric suffixes once
// you have ten or more backups (".10" sorts before ".2"). For logrotate's
// default numeric naming use [Logrotate] instead; for time-suffixed names use
// [Lumberjack].
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

// LogrotateOption is a functional option for [Logrotate].
type LogrotateOption func(*logrotateSource)

// WithLogrotateSkippedHook installs a callback that fires once per backup
// file Logrotate recognises but cannot expose to the tailer. The hook is
// invoked synchronously from [Source.Enumerate]; it must not block.
//
// reason is a short tag identifying why the file was skipped:
//   - "compressed": the file is a compressed logrotate backup
//     (<active>.<N>.gz). gotail does not decompress on read.
func WithLogrotateSkippedHook(fn func(path, reason string)) LogrotateOption {
	return func(s *logrotateSource) { s.skipped = fn }
}

// Logrotate returns a [Source] for log files rotated by logrotate's default
// numeric naming scheme: <activePath>.1, <activePath>.2, ... where ".1" is
// the most recent backup and the highest-numbered file is the oldest.
//
// Backups are returned oldest-first (highest N first); activePath is last.
// Files matching <activePath>.<N>.gz are not enumerated; use
// [WithLogrotateSkippedHook] to observe them.
//
// For logrotate with the dateext directive (which produces names like
// app.log-20240315) use [Lumberjack] instead.
func Logrotate(activePath string, opts ...LogrotateOption) Source {
	s := &logrotateSource{activePath: activePath}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type logrotateSource struct {
	activePath string
	skipped    func(path, reason string) // optional
}

func (s *logrotateSource) Enumerate(_ context.Context) ([]string, error) {
	matches, err := filepath.Glob(s.activePath + ".*")
	if err != nil {
		return nil, fmt.Errorf("tail: logrotate glob: %w", err)
	}

	type backup struct {
		path string
		age  int // higher = older
	}
	var backups []backup
	prefix := s.activePath + "."

	for _, p := range matches {
		if p == s.activePath {
			continue
		}
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		suffix := p[len(prefix):]

		if compressed, ok := strings.CutSuffix(suffix, ".gz"); ok {
			if _, err := strconv.Atoi(compressed); err == nil {
				if s.skipped != nil {
					s.skipped(p, "compressed")
				}
			}
			continue
		}

		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue // not a numeric backup; ignore
		}
		backups = append(backups, backup{path: p, age: n})
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].age > backups[j].age // oldest (largest N) first
	})

	result := make([]string, 0, len(backups)+1)
	for _, b := range backups {
		result = append(result, b.path)
	}
	return append(result, s.activePath), nil
}
