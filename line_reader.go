package tail

import (
	"bufio"
	"bytes"
	"io"
	"time"
)

// LineReader provides a way to transparently read
// \n or \r\n delimited lines across multiple files.
// The only method that is safe to call in parallel
// to other methods is Close().
type LineReader struct {
	onErr ErrorHandler
	c     Config

	r Watcher

	s  WaitStatus
	br *bufio.Reader

	lastBytes []byte

	stop chan struct{}

	err error
}

// NewLineReader returns a LineReader that has an underlying
// Watcher created from c and will run unexpected errors through
// ErrorHandler h. If the error is an EOF or file not found error,
// it will not be passed to the error handler. If h is nil,
// errors will be ignored and will automatically retry.
func NewLineReader(c Config, h ErrorHandler) (*LineReader, error) {
	if h == nil {
		h = DiscardErrorHandler
	}

	r, err := NewPollingWatcher(c)
	if err != nil {
		return nil, err
	}

	return &LineReader{
		onErr: h,
		r:     r,
		c:     c,
		stop:  make(chan struct{}),
	}, nil
}

func (l *LineReader) sleep(t time.Duration) bool {
	if t == 0 {
		select {
		case <-l.stop:
			return false
		default:
			return true
		}
	}

	select {
	case <-l.stop:
		return false
	case <-time.After(t):
		return true
	}
}

func (l *LineReader) Next() bool {

	var sleepTime time.Duration

	l.lastBytes = nil

	for {
		var b []byte
		var err error

		if l.err != nil || !l.sleep(sleepTime) {
			return false
		}

		sleepTime = l.c.Interval

		if l.br == nil {
			goto Wait
		}

		b, err = l.br.ReadBytes('\n')
		l.s.State.Position += int64(len(b))

		if len(b) > 0 {
			// Avoid an allocation if lastBytes is nil.
			if l.lastBytes != nil {
				l.lastBytes = append(l.lastBytes, b...)
			} else {
				l.lastBytes = b
			}
		}

		if err == nil {
			break
		}

		if err != io.EOF {
			l.err = l.onErr(err)
			sleepTime = time.Second
			continue
		}

		// The error was an EOF, so wait for more data.
		if l.c.StopAtEOF {
			l.err = err
			continue
		}

	Wait:
		s, closed, err := l.r.Wait()
		if closed {
			return false
		}

		l.s = s

		if err != nil {
			l.err = l.onErr(err)
			sleepTime = time.Second
			continue
		}

		if s.ReOpened {
			l.br = bufio.NewReader(s.File)
			continue
		}
	}

	// MUST have a \n suffix if it makes it to this point, so test \r.
	trim := len(l.lastBytes) - 1
	if bytes.HasSuffix(l.lastBytes, []byte{'\r', '\n'}) {
		trim--
	}
	l.lastBytes = l.lastBytes[:trim]

	// Don't touch the position, because if we want to resume where we
	// left off, it should point to the start of the next line.
	return true
}

func (l *LineReader) handleError(err error) {
	l.onErr(err)
}

func (l *LineReader) Bytes() []byte {
	return l.lastBytes
}

// Err returns any error that occurred that caused Next to
// return false. If it's set, it will generally be what was
// returned by the ErrorHandler.
func (l *LineReader) Err() error {
	return l.err
}

// Close cleans up any resources and should only be called once.
func (l *LineReader) Close() error {
	select {
	case <-l.stop:
		break
	default:
		close(l.stop)
	}

	return l.r.Close()
}

func (l *LineReader) FileState() FileState {
	return l.s.State
}
