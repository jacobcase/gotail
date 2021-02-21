package tail

import (
	"os"
	"time"
)

// ErrorHandler allows you to
type ErrorHandler func(err error) error

// DiscardErrorHandler ignores all errors and always returns nil.
func DiscardErrorHandler(error) error {
	return nil
}

// Config is shared among a few types in this package to configure
// what and how to tail a file.
type Config struct {
	// Path should be the location to a regular file. This value is
	// not validated and is passed directly to os.Open().
	Path string

	// Interval is used for a few operations. For file polling, it is
	// how frequently to check for new data. For the LineReader, it is
	// also how long to wait before retrying.
	Interval time.Duration

	// Whence can be set to one of the Seek constants from the IO package.
	// It only applies to the first file opened, as subsequent files will always be
	// read from the beginning. io.SeekCurrent will behave the same as io.SeekStart.
	// This will also be disregarded if the file doesn't initially exist on disk.
	Whence int

	// StartState is optional for resuming the first file opened from where it left
	// off if the FileState matches.
	StartState *FileState

	// StopAtEOF will cause a tail to exit when it gets the first EOF.
	// Useful for consumers to build tests.
	StopAtEOF bool
}

// WaitStatus is the result of  Watcher.Wait and should contain enough
// information for callers to setup for the next file Read.
type WaitStatus struct {
	// State contains the latest open file size, position of the
	// descriptor, and file inode. This can be used to preserve
	// the position between application starts for the same file.
	State FileState

	// File is a ready to use file the Watcher has determined
	// is next to be read from. This file does NOT need to be
	// closed by the consumer, as it should always be closed
	// when a Watcher no longer considers it the latest to read
	// from or the Watcher is closed.
	File *os.File

	// ReOpened, if true, indicates the file returned has just been
	// opened. This will also be true for the first file opened, even
	// though there wasn't one previously.
	ReOpened bool
}

// Watcher provides a simple interface to handle reading rotated files.
type Watcher interface {
	// Wait will block until there is more data to read, the watcher
	// is closed, or there was an error checking if there was more data
	// to read. Wait should always be safe to call again if there was
	// an error previously, but calling again when closed returns true
	// should be avoided.
	Wait() (s WaitStatus, closed bool, err error)

	// Close will stop the Watcher, cleanup any resources, and
	// return the result of closing the currently open file if one
	// is open.
	Close() error
}
