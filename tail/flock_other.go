//go:build !unix && !windows

package tail

import "errors"

type flock struct{}

func acquireFlock(_ string) (*flock, error) {
	return nil, errors.New("tail: file locking is not supported on this platform")
}

func (l *flock) release() error { return nil }
