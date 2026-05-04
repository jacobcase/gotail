package watch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"
)

// Position describes a point in a file series. It is a pure value; no I/O.
type Position struct {
	File   string `json:"file"`
	Inode  uint64 `json:"inode,string"`
	Offset int64  `json:"offset,string"`
}

// IsZero reports whether p is the zero value.
func (p Position) IsZero() bool {
	return p == (Position{})
}

// Event describes a state transition observed by a [Watcher]. The Watcher
// emits events; it does not yield bytes. The consumer (typically [LineReader])
// opens its own file handle against Event.Path for normal reads.
//
// On rotation, PreRotation provides access to trailing bytes from the
// rotated-out file via a Reader the Watcher keeps open until the next Wait
// call — preserving the race-aware drain behaviour of v1.
type Event struct {
	// Path is the current active path; only changes when ReOpened is true.
	Path string
	// Pos is the logical position at the time of the event.
	Pos Position
	// ReOpened is true on the first open or when rotation is detected.
	ReOpened bool
	// Truncated is true when the file size dropped below the current position.
	Truncated bool
	// PreRotation is non-nil when ReOpened was triggered by rotation with
	// pending unread bytes in the old file. Valid until the next Wait or Close.
	PreRotation *PreRotation
}

// PreRotation grants access to trailing bytes on the rotated-out file.
// Reader is valid until the next call to [Watcher.Wait] or [Watcher.Close].
type PreRotation struct {
	Reader    io.Reader
	FinalSize int64
	StartPos  int64
}

// Watcher emits events about file state transitions. Production implementations
// own a *os.File internally and manage its lifecycle. Tests can use [FakeWatcher].
type Watcher interface {
	Wait(ctx context.Context) (Event, error)
	Close() error
}

// Config configures a Watcher.
type Config struct {
	// Path is the file to watch. Must be non-empty.
	Path string
	// Interval is the poll interval. Zero defaults to 1 second.
	Interval time.Duration
	// Whence is io.SeekStart, io.SeekCurrent, or io.SeekEnd. Applies only
	// to the first file opened; subsequent files always start at offset 0.
	Whence int
	// Resume, if non-nil, is an optional resume point subject to inode match.
	Resume *Position
	// StopAtEOF causes the Watcher to return io.EOF once the file is exhausted
	// instead of blocking forever.
	StopAtEOF bool
	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
	// NoInodeCheck disables the inode equality check on resume and rotation
	// detection. Use on Windows ReFS, FUSE mounts, or other filesystems with
	// unstable inodes.
	NoInodeCheck bool
}

// New returns a Watcher. It attempts NewFsnotify first; if that returns
// [ErrUnsupported] it falls back to [NewPolling].
func New(c Config) (Watcher, error) {
	w, err := NewFsnotify(c)
	if err == nil {
		return w, nil
	}
	if errors.Is(err, ErrUnsupported) {
		return NewPolling(c)
	}
	return nil, err
}

// StatInode returns a stable file identity for the file at path.
// On Unix it is the inode from stat(2); on Windows it is the file index from
// GetFileInformationByHandle, which requires opening the file. Returns 0 on
// filesystems where a stable identity is not available (e.g., ReFS, some
// network filesystems).
func StatInode(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return fileID(f), nil
}

var (
	// ErrUnsupported is returned by [NewFsnotify] on builds or platforms
	// where the fsnotify backend is not available.
	ErrUnsupported = errors.New("watch: unsupported on this platform/build")
	// ErrInodeMismatch is returned when the file's inode no longer matches
	// the resume point.
	ErrInodeMismatch = errors.New("watch: file inode no longer matches resume point")
	// ErrTruncated is returned when the file was truncated below the current
	// position.
	ErrTruncated = errors.New("watch: file was truncated below current position")
	// ErrLineTooLong is returned by [LineReader.Next] when a line exceeds MaxLine.
	ErrLineTooLong = errors.New("watch: line exceeds MaxLine")
)
