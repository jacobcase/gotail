package tail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

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

// SyncMode controls when a [FileCursor] flushes buffered checkpoints to disk.
type SyncMode int

const (
	// SyncAlways fsyncs on every [FileCursor.Save] call. This is the default
	// and the safest mode: the cursor is never more than one Save behind disk.
	SyncAlways SyncMode = iota
	// SyncOnCommit buffers the latest checkpoint in memory; an explicit call to
	// [Syncer.Sync] (type-asserted from the [Cursor] interface) flushes it.
	// The Tailer's Commit calls only Save; the caller controls when Sync runs.
	SyncOnCommit
	// SyncBackground buffers the latest checkpoint in memory and flushes it in
	// the background at most every [DefaultSyncBackgroundInterval]. The background
	// goroutine starts in [NewFileCursor] and stops when [Cursor.Close] is called.
	// Use [goleak] in tests to verify the goroutine terminates cleanly.
	SyncBackground
)

// DefaultSyncBackgroundInterval is the flush interval used by [SyncBackground]
// when no explicit interval is configured. Decision #23: 1 second matches the
// default poll interval and bounds cursor staleness to one second.
const DefaultSyncBackgroundInterval = time.Second

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

// Syncer is an extension interface optionally implemented by [Cursor] values.
// [FileCursor] implements Syncer when configured with [SyncOnCommit] or
// [SyncBackground]. [Tailer.Commit] calls only [Cursor.Save]; the caller
// controls flushing by type-asserting to Syncer and calling Sync.
type Syncer interface {
	// Sync flushes any buffered checkpoint to disk. It is a no-op when no
	// checkpoint is buffered (i.e. after construction or after the previous
	// Sync consumed the buffer).
	Sync(ctx context.Context) error
}

// Checkpoint is what gets persisted. Meta is opaque user data, stored as a
// raw JSON value alongside the position. Load callers unmarshal it themselves.
type Checkpoint struct {
	Pos  Position        `json:"pos"`
	Meta json.RawMessage `json:"meta,omitempty"`
}

// CursorMigrator is the function type for on-load cursor migration.
// It receives the on-disk version number and the raw file bytes, and
// must return a migrated [Checkpoint] in the current schema.
//
// If the migrator returns an error, [FileCursor.Load] wraps it so that
// [errors.Is](err, [ErrUnsupportedCursorVersion]) still holds — existing
// callers' error branches remain unchanged.
//
// On success, Load writes the migrated checkpoint back to disk so that
// subsequent loads bypass the migrator.
type CursorMigrator func(version int, raw []byte) (Checkpoint, error)

// FileCursorOption is a functional option for [NewFileCursor].
type FileCursorOption func(*fileCursorOpts)

type fileCursorOpts struct {
	dirSync      bool
	fileMode     os.FileMode
	flockPath    string
	migrate      CursorMigrator
	syncMode     SyncMode
	syncInterval time.Duration // used only by SyncBackground; 0 = DefaultSyncBackgroundInterval
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

// WithCursorMigration registers a [CursorMigrator] that [FileCursor.Load]
// calls when the on-disk cursor file has an unsupported version (zero or
// higher than the current schema). Without this option, Load returns
// [ErrUnsupportedCursorVersion] on any version mismatch.
//
// On a successful migration Load rewrites the cursor file in the current
// schema so that subsequent loads skip the migrator.
func WithCursorMigration(fn CursorMigrator) FileCursorOption {
	return func(o *fileCursorOpts) { o.migrate = fn }
}

// WithSyncMode sets the flush strategy for a [FileCursor].
// Default is [SyncAlways]. See [SyncMode] for semantics.
func WithSyncMode(m SyncMode) FileCursorOption {
	return func(o *fileCursorOpts) { o.syncMode = m }
}

// WithSyncBackgroundInterval overrides the flush interval used by
// [SyncBackground]. Zero or negative values use [DefaultSyncBackgroundInterval].
// Ignored when the sync mode is not [SyncBackground].
func WithSyncBackgroundInterval(d time.Duration) FileCursorOption {
	return func(o *fileCursorOpts) { o.syncInterval = d }
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
//
// When configured with [SyncOnCommit] or [SyncBackground], FileCursor
// implements the [Syncer] extension interface.
type FileCursor struct {
	path string
	opts fileCursorOpts
	lk   *flock // nil when WithFlock was not used or path was empty

	// mu guards pending and dirty for SyncOnCommit / SyncBackground.
	mu      sync.Mutex
	pending Checkpoint
	dirty   bool

	// stopBg and bgDone are used only by SyncBackground.
	stopBg chan struct{}
	bgDone chan struct{}
}

// NewFileCursor opens (or creates) a cursor at path. The containing directory
// must already exist. If [WithFlock] is provided with a non-empty lock path,
// the lock is acquired before returning and held until [Cursor.Close].
//
// When [WithSyncMode]([SyncBackground]) is set, a background goroutine starts
// that flushes the buffered checkpoint at [DefaultSyncBackgroundInterval].
// The goroutine terminates when [Cursor.Close] is called.
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
	if o.syncMode == SyncBackground {
		interval := o.syncInterval
		if interval <= 0 {
			interval = DefaultSyncBackgroundInterval
		}
		c.stopBg = make(chan struct{})
		c.bgDone = make(chan struct{})
		go c.backgroundFlusher(interval)
	}
	return c, nil
}

// backgroundFlusher runs in a goroutine and flushes dirty checkpoints at
// the configured interval. It exits when stopBg is closed.
func (c *FileCursor) backgroundFlusher(interval time.Duration) {
	defer close(c.bgDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = c.Sync(context.Background())
		case <-c.stopBg:
			// Final flush before exit.
			_ = c.Sync(context.Background())
			return
		}
	}
}

func (c *FileCursor) Load(ctx context.Context) (Checkpoint, bool, error) {
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
		if c.opts.migrate == nil {
			return Checkpoint{}, false, fmt.Errorf(
				"tail: cursor %s has version %d, this build expects %d: %w",
				c.path, cf.Version, cursorVersion, ErrUnsupportedCursorVersion)
		}
		// Call the user-supplied migrator.
		migrated, merr := c.opts.migrate(cf.Version, data)
		if merr != nil {
			return Checkpoint{}, false, fmt.Errorf(
				"tail: cursor %s migration from version %d failed: %w: %w",
				c.path, cf.Version, merr, ErrUnsupportedCursorVersion)
		}
		// Persist the migrated checkpoint so subsequent loads skip the migrator.
		if werr := c.Save(ctx, migrated); werr != nil {
			return Checkpoint{}, false, fmt.Errorf("tail: cursor %s migration save: %w", c.path, werr)
		}
		return migrated, true, nil
	}
	return Checkpoint{Pos: cf.Pos, Meta: cf.Meta}, true, nil
}

func (c *FileCursor) Save(ctx context.Context, cp Checkpoint) error {
	if len(cp.Meta) > maxRawMetaBytes {
		return fmt.Errorf("tail: raw meta size %d exceeds %d-byte limit", len(cp.Meta), maxRawMetaBytes)
	}
	switch c.opts.syncMode {
	case SyncOnCommit, SyncBackground:
		// Buffer; don't write to disk yet.
		c.mu.Lock()
		c.pending = cp
		c.dirty = true
		c.mu.Unlock()
		return nil
	default: // SyncAlways
		return c.flush(cp)
	}
}

// flush performs the actual atomic write to disk. Used by Save (SyncAlways)
// and Sync (SyncOnCommit / SyncBackground).
func (c *FileCursor) flush(cp Checkpoint) error {
	data, err := json.Marshal(cursorFile{Pos: cp.Pos, Meta: cp.Meta, Version: cursorVersion})
	if err != nil {
		return fmt.Errorf("tail: marshal cursor: %w", err)
	}
	return atomicwrite.Write(c.path, data, c.opts.fileMode, c.opts.dirSync)
}

// Sync flushes the buffered checkpoint to disk. It is a no-op when the buffer
// is not dirty or when the cursor is in [SyncAlways] mode (every Save already
// fsyncs). Sync implements the [Syncer] extension interface.
func (c *FileCursor) Sync(_ context.Context) error {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	cp := c.pending
	c.dirty = false
	c.mu.Unlock()
	return c.flush(cp)
}

func (c *FileCursor) Close() error {
	// Stop the background flusher goroutine if running.
	if c.stopBg != nil {
		close(c.stopBg)
		<-c.bgDone
	}
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
