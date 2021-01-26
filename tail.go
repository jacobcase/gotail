package tail

import (
	"io"
	"os"
	"sync"
	"time"
)

type ErrorHandler func(err error)

// Tailer is a common interface for all implementations to support.
type Tailer interface {
	io.ReadCloser
}

type PollerConfig struct {
	// FilePath should be the location to a regular file. This value is
	// not validated and is passed directly to os.Open().
	FilePath string

	// Interval is how frequently to stat the file and check for more data.
	Interval time.Duration

	// OnError may be nil. If so, the error is ignored and polling starts over
	// and tries forever. It is safe to call Close() on the poller in the handler.
	OnError ErrorHandler

	// Whence can be set to one of the Seek constants from the IO package.
	// It only applies to the first file opened, as subsequent files will always be
	// read from the beginning. io.SeekCurrent will behave the same as io.SeedStart.
	Whence int
}

func NewPoller(c PollerConfig) Tailer {

	if c.OnError == nil {
		c.OnError = func(error) {}
	}

	if c.Interval <= 0 {
		c.Interval = time.Second
	}

	p := &Poller{
		c:      c,
		timer:  time.NewTimer(0),
		cancel: make(chan struct{}),
	}

	// drain the timer so it's ready for Reset()
	if !p.timer.Stop() {
		<-p.timer.C
	}

	return p
}

// Poller is a simple file tail implementation that polls files
// for changes.
type Poller struct {
	c PollerConfig

	f         *os.File
	bytesRead int64
	timer     *time.Timer

	mu     sync.Mutex
	cancel chan struct{}
	closed bool
}

// Read passes the byte slice to the underlying *os.File from FilePath in the config.
// If the file file doesn't exist yet, Read blocks until it can be opened or Close()
// is called. If the file is already open, it continues reading. If Read encounters
// an EOF with no bytes read, it will block for changes or until Close() is called.
// If the size of the open file grows, try to read again. Otherwise, see if the file at
// the configured path is still the same as the open one. If not, Read will close the old
// file and open the new one.
// If Read to the underlying file returns an error other than EOF, the Read results will be
// returned as is.
func (p *Poller) Read(b []byte) (n int, err error) {
	f, closed := p.getFile()

	for !closed {

		if f == nil {
			f, closed = p.reopenForever()
			continue
		}

		n, err = p.f.Read(b)

		p.bytesRead += int64(n)

		// Pass all errors through EXCEPT for EOF with no bytes read. If we did read some bytes
		// and get EOF, return the bytes and wait for the next read to handle the EOF.
		if err == io.EOF {
			if n == 0 {
				f, closed = p.waitForMore()
				continue
			}
			return n, nil
		}
		return n, err
	}
	return 0, os.ErrClosed
}

func (p *Poller) waitForMore() (f *os.File, closed bool) {

	for {

		p.timer.Reset(p.c.Interval)

		select {
		case <-p.cancel:
			return p.getFile()
		case <-p.timer.C:
			break
		}

		f, closed = p.getFile()
		if closed {
			return f, closed
		}

		statCurrent, err := f.Stat()
		if err != nil {
			p.c.OnError(err)
			continue
		}

		if statCurrent.Size() > p.bytesRead {
			return f, false
		}

		statNamed, err := os.Stat(p.c.FilePath)
		if err != nil {
			p.c.OnError(err)
			continue
		}
		if os.SameFile(statCurrent, statNamed) {
			continue
		}

		return p.reopenForever()
	}
}

func (p *Poller) getFile() (f *os.File, closed bool) {
	p.mu.Lock()
	f, closed = p.f, p.closed
	p.mu.Unlock()
	return
}

func (p *Poller) reopenForever() (f *os.File, closed bool) {
	for {
		f, err := os.Open(p.c.FilePath)
		if err == nil && p.c.Whence == io.SeekStart {
			return f, p.replaceFileOrClose(f)
		}

		_, err = f.Seek(0, p.c.Whence)
		if err == nil {
			p.c.Whence = io.SeekStart
			return f, p.replaceFileOrClose(f)
		}

		p.c.OnError(err)

		p.timer.Reset(p.c.Interval)
		select {
		case <-p.cancel:
			return p.getFile()
		case <-p.timer.C:
			break
		}
	}
}

func (p *Poller) replaceFileOrClose(f *os.File) (closed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		f.Close()
	} else {
		if oldF := p.f; oldF != nil {
			oldF.Close()
		}
		p.f = f
	}
	return p.closed
}

// Close stops the file poller and closes the underlying file.
// It is safe to call during a Read().
// Close will return the result of calling the last open file's Close method.
func (p *Poller) Close() error {

	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.cancel:
	default:
		close(p.cancel)
	}

	p.closed = true
	if p.f != nil {
		return p.f.Close()
	}

	return nil
}
