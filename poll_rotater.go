package tail

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type WaitStatus struct {
	State    FileState
	File     *os.File
	ReOpened bool
}

type Rotater interface {
	Wait() (s WaitStatus, closed bool, err error)
	Close() error
}

type PollerConfig struct {
	// Path should be the location to a regular file. This value is
	// not validated and is passed directly to os.Open().
	Path string

	// Interval is how frequently to stat the file and check for more data.
	Interval time.Duration

	// FirstWhence can be set to one of the Seek constants from the IO package.
	// It only applies to the first file opened, as subsequent files will always be
	// read from the beginning. io.SeekCurrent will behave the same as io.SeekStart.
	// This will also be disregarded if the file doesn't initially exist on disk.
	FirstWhence int

	// StartState is optional for resuming the first file opened from where it left
	// off if the FileState matches.
	StartState *FileState
}

var _ Rotater = (*PollingRotater)(nil)

type PollingRotater struct {
	c PollerConfig

	timer *time.Timer
	f     *os.File

	cancel chan struct{}
	closed bool

	mu sync.Mutex
}

func NewPollingRotater(c PollerConfig) (*PollingRotater, error) {
	if !(c.FirstWhence == io.SeekStart ||
		c.FirstWhence == io.SeekCurrent ||
		c.FirstWhence == io.SeekEnd) {
		return nil, fmt.Errorf("config value for whence of %v is invalid", c.FirstWhence)
	}

	if c.Interval < 0 {
		return nil, errors.New("config value for interval cannot be negative")
	} else if c.Interval == 0 {
		c.Interval = time.Second
	}

	if c.Path == "" {
		return nil, errors.New("config value for path cannot be empty")
	}

	p := &PollingRotater{
		c:      c,
		timer:  time.NewTimer(0),
		cancel: make(chan struct{}),
	}
	// No way to create a timer without an initial tick, so drain it.
	<-p.timer.C
	return p, nil
}

func (p *PollingRotater) Wait() (s WaitStatus, closed bool, err error) {
	p.mu.Lock()
	defer func() {
		if !p.timer.Stop() {
			select {
			case <-p.timer.C:
			default:
			}
		}
		p.mu.Unlock()
	}()

	for {
		p.timer.Reset(p.c.Interval)

		p.mu.Unlock()
		select {
		case <-p.cancel:
		case <-p.timer.C:
		}
		p.mu.Lock()

		if p.closed {
			return s, true, nil
		}

		if p.f == nil {
			f, err := p.openAndSeek()
			if os.IsNotExist(err) {
				p.c.FirstWhence = io.SeekStart
				continue
			}

			if err != nil {
				return s, p.closed, err
			}

			s.State, err = NewFileState(f)
			if err != nil {
				return s, p.closed, err
			}

			p.f = f
			s.File = f
			s.ReOpened = true
			return s, false, err
		}

		s.File = p.f
		s.State, err = NewFileState(p.f)
		if err != nil {
			return s, false, err
		}

		if s.State.Size > s.State.Position {
			return s, false, nil
		}

		stateNamed, err := NewFileStateFromPath(p.c.Path)
		// Inode should never be the same if they are two different files
		// since we have the old file open, keeping a reference to it on
		// disk. Usually rotation moves files anyways, which should keep
		// the inode in most situations.
		if err == nil && s.State.Inode == stateNamed.Inode {
			continue
		} else if os.IsNotExist(err) {
			continue
		} else {
			return s, false, err
		}

		// If we get here, the named file is different from the one
		// currently open (it was rotated). However, it is possible
		// for there to be a race. Between when the open file is checked
		// for size, and the check for a replacement file, the current
		// open file could have had bytes written to it before rotation.
		// So to make sure we get all the data, ignore the latest file
		// on disk until our position matches the size of the old file
		// by checking the size again.
		s.State, err = NewFileState(p.f)
		if err != nil {
			return s, false, err
		}

		if s.State.Size > s.State.Position {
			return s, false, nil
		}

		// There is a new file on disk and we have read up to the
		// end of the open one, so close it and reset for the next.
		p.f.Close()
		p.f = nil
	}
}

func (p *PollingRotater) openAndSeek() (f *os.File, err error) {
	f, err = os.Open(p.c.Path)
	if err != nil {
		return nil, err
	}

	if p.c.StartState != nil {
		_, _, err = p.c.StartState.SeekIfMatches(f)
		if err != nil {
			f.Close()
			return nil, err
		}

		p.c.StartState = nil
		p.c.FirstWhence = io.SeekStart
	} else if p.c.FirstWhence != io.SeekStart {
		_, err = f.Seek(0, p.c.FirstWhence)
		if err != nil {
			f.Close()
			return nil, err
		}
		p.c.FirstWhence = io.SeekStart
	}

	return f, nil
}

func (p *PollingRotater) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.cancel)
	}
	if p.f != nil {
		return p.f.Close()
	}
	return nil
}
