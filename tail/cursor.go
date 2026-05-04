package tail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/jacobcase/gotail/v2/internal/atomicwrite"
)

// cursorVersion is the schema version this build writes and the only version
// it accepts on Load. Bumping requires migration support.
const cursorVersion = 1

// ErrUnsupportedCursorVersion is returned by [FileCursor.Load] when the
// stored cursor file has a Version that this build does not understand —
// either zero (corrupt or hand-edited) or higher than [cursorVersion]
// (written by a newer build). Callers can use [errors.Is] to detect it.
var ErrUnsupportedCursorVersion = errors.New("tail: unsupported cursor version")

// Cursor persists a [Checkpoint] (Position + opaque user metadata).
type Cursor interface {
	// Load returns the most-recently saved checkpoint. The bool is false when
	// no checkpoint exists yet (first run). A nil error with false is normal.
	Load(ctx context.Context) (Checkpoint, bool, error)
	// Save atomically replaces the stored checkpoint.
	Save(ctx context.Context, c Checkpoint) error
	// Close releases any resources held by the cursor (e.g. file locks).
	Close() error
}

// Checkpoint is what gets persisted. Meta is opaque user data, stored as a
// raw JSON value alongside the position. Load callers unmarshal it themselves.
type Checkpoint struct {
	Pos  Position        `json:"pos"`
	Meta json.RawMessage `json:"meta,omitempty"`
}

// FileCursorOption is a functional option for [NewFileCursor].
type FileCursorOption func(*fileCursorOpts)

type fileCursorOpts struct {
	dirSync   bool
	fileMode  os.FileMode
	flockPath string
}

// WithDirSync controls whether [FileCursor.Save] fsyncs the containing
// directory after the atomic rename. Default is true (on), required for
// power-loss durability of the rename on ext4/xfs.
func WithDirSync(on bool) FileCursorOption {
	return func(o *fileCursorOpts) { o.dirSync = on }
}

// WithFileMode sets the permission bits for the cursor file. Default 0o600.
func WithFileMode(mode os.FileMode) FileCursorOption {
	return func(o *fileCursorOpts) { o.fileMode = mode }
}

// WithFlock acquires an exclusive advisory lock on lockPath before returning
// from [NewFileCursor], and releases it in [Cursor.Close]. An empty lockPath
// is a no-op. The lock file must be a sibling of the cursor file — never use
// the cursor file itself, because rename-over-open silently drops POSIX locks.
func WithFlock(lockPath string) FileCursorOption {
	return func(o *fileCursorOpts) { o.flockPath = lockPath }
}

// cursorFile is the on-disk JSON format.
type cursorFile struct {
	Pos     Position        `json:"pos"`
	Meta    json.RawMessage `json:"meta,omitempty"`
	Version int             `json:"version"`
}

// maxRawMetaBytes caps the user-supplied raw JSON. The on-disk envelope
// (Pos + Version) adds a small constant overhead on top of this.
const maxRawMetaBytes = 64 * 1024

// FileCursor atomically persists checkpoints to a JSON file using a
// write-to-tmp + fsync + rename sequence.
type FileCursor struct {
	path string
	opts fileCursorOpts
	lk   *flock // nil when WithFlock was not used or path was empty
}

// NewFileCursor opens (or creates) a cursor at path. The containing directory
// must already exist. If [WithFlock] is provided with a non-empty lock path,
// the lock is acquired before returning and held until [Cursor.Close].
func NewFileCursor(path string, opts ...FileCursorOption) (Cursor, error) {
	o := fileCursorOpts{
		dirSync:  true,
		fileMode: 0o600,
	}
	for _, fn := range opts {
		fn(&o)
	}
	c := &FileCursor{path: path, opts: o}
	if o.flockPath != "" {
		lk, err := acquireFlock(o.flockPath)
		if err != nil {
			return nil, err
		}
		c.lk = lk
	}
	return c, nil
}

func (c *FileCursor) Load(_ context.Context) (Checkpoint, bool, error) {
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("tail: read cursor %s: %w", c.path, err)
	}
	var cf cursorFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return Checkpoint{}, false, fmt.Errorf("tail: parse cursor %s: %w", c.path, err)
	}
	if cf.Version != cursorVersion {
		return Checkpoint{}, false, fmt.Errorf(
			"tail: cursor %s has version %d, this build expects %d: %w",
			c.path, cf.Version, cursorVersion, ErrUnsupportedCursorVersion)
	}
	return Checkpoint{Pos: cf.Pos, Meta: cf.Meta}, true, nil
}

func (c *FileCursor) Save(_ context.Context, cp Checkpoint) error {
	if len(cp.Meta) > maxRawMetaBytes {
		return fmt.Errorf("tail: raw meta size %d exceeds %d-byte limit", len(cp.Meta), maxRawMetaBytes)
	}
	data, err := json.Marshal(cursorFile{Pos: cp.Pos, Meta: cp.Meta, Version: cursorVersion})
	if err != nil {
		return fmt.Errorf("tail: marshal cursor: %w", err)
	}
	return atomicwrite.Write(c.path, data, c.opts.fileMode, c.opts.dirSync)
}

func (c *FileCursor) Close() error {
	if c.lk != nil {
		return c.lk.release()
	}
	return nil
}

// MemoryCursor is an in-memory [Cursor] for tests.
type memoryCursor struct {
	mu   sync.Mutex
	cp   Checkpoint
	have bool
}

// NewMemoryCursor returns an in-memory [Cursor] suitable for tests.
func NewMemoryCursor() Cursor {
	return &memoryCursor{}
}

func (m *memoryCursor) Load(_ context.Context) (Checkpoint, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cp, m.have, nil
}

func (m *memoryCursor) Save(_ context.Context, cp Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cp = cp
	m.have = true
	return nil
}

func (m *memoryCursor) Close() error { return nil }
