package tail

import (
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// TODO: file creation time (or birth time) isn't universally supported the same
// between filesystems and stat doesn't always show it, but in the future
// it may be worth supporting an optional creation time field.

// FileState describes some details about a regular file that can be used
// to compare it with another file on disk for a best guess on if they are
// the same file. It can also store the position of a file descriptor
// to allow seeking if reopening later to continue where it last left off.
type FileState struct {
	Size     int64  `json:",string"`
	Position int64  `json:",string"`
	Inode    uint64 `json:",string"`
}

// SeekIfMatches will try to determine if this FileState matches that of the file,
// which means they must have a matching Inode and the size of f must be at least as
// big as this FileState's Position. Otherwise it does nothing. The returned SeekInfo
// is always valid for f if the error is nil, though the Position is not updated so
// if the descriptor of f points beyond the start of the file, Position will
// need to be updated outside this method.
func (s *FileState) SeekIfMatches(f *os.File) (fs FileState, matches bool, err error) {
	newState, err := NewFileState(f)
	if err != nil {
		return FileState{}, false, err
	}

	if s.Inode != newState.Inode {
		return newState, false, nil
	}

	// Inode can be reused or file could be truncated. Truncation isn't really supported
	// by this module anyways. Checking the size is another guard against thinking
	// a different file is the same.
	if s.Position > newState.Size {
		return newState, false, nil
	}

	newState.Position, err = f.Seek(s.Position, io.SeekStart)
	if err != nil {
		return FileState{}, true, err
	}

	return newState, true, err
}

func (s *FileState) readInfo(i os.FileInfo) error {
	s.Size = i.Size()

	switch stat_t := i.Sys().(type) {
	case *unix.Stat_t:
		s.Inode = stat_t.Ino
	case *syscall.Stat_t:
		s.Inode = stat_t.Ino
	default:
		return errors.New("file stat isn't *unix.Stat_t type")
	}
	return nil
}

// NewFileState will initialize a FileState with the inode, size, and position
// of the provided file. Currently does not support windows, or anything that
// isn't a *syscall.Stat_t or *unix.Stat_t in the underlying stat.
func NewFileState(f *os.File) (FileState, error) {
	stat, err := f.Stat()
	if err != nil {
		return FileState{}, err
	}

	var inode uint64

	switch stat_t := stat.Sys().(type) {
	case *unix.Stat_t:
		inode = stat_t.Ino
	case *syscall.Stat_t:
		inode = stat_t.Ino
	default:
		return FileState{}, errors.New("file stat isn't *unix.Stat_t type")
	}

	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return FileState{}, err
	}

	return FileState{
		Size:     stat.Size(),
		Inode:    inode,
		Position: pos,
	}, nil
}

func NewFileStateFromPath(p string) (*FileState, error) {
	stat, err := os.Stat(p)
	if err != nil {
		return nil, err
	}

	var state FileState
	return &state, state.readInfo(stat)
}
