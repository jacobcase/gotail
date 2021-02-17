package tail

import (
	"bufio"
	"bytes"
	"io"
	"time"
)

type ErrorHandler func(err error)

type LineReader struct {
	onErr ErrorHandler

	r Rotater

	s  WaitStatus
	br *bufio.Reader

	lastBytes []byte

	stop chan struct{}
}

func NewLineReader(r Rotater, errHandler ErrorHandler) *LineReader {
	return &LineReader{
		onErr: errHandler,
		r:     r,
		// An empty reader makes it safe to read and cause the
		// Next loop to start trying to open the file.
		br:   bufio.NewReader(bytes.NewReader(nil)),
		stop: make(chan struct{}),
	}
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

	for l.sleep(sleepTime) {
		sleepTime = time.Second

		tb, err := l.br.ReadBytes('\n')
		l.s.State.Position += int64(len(tb))

		// Avoid an allocation if lastBytes is nil.
		if l.lastBytes != nil {
			l.lastBytes = append(l.lastBytes, tb...)
		} else {
			l.lastBytes = tb
		}

		if err == nil {
			break
		}

		if err != io.EOF {
			l.onErr(err)
			sleepTime = time.Second
			continue
		}

		// The error was an EOF, so wait for more data.
		s, closed, err := l.r.Wait()
		if closed {
			return false
		}

		l.s = s

		if err != nil {
			l.onErr(err)
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
