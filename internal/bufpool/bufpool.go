// Package bufpool provides a sync.Pool of reusable byte slices.
package bufpool

import "sync"

var pool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

// Get returns a *[]byte from the pool. The slice is reset to length zero but
// retains its capacity. Callers must return it with Put when done.
func Get() *[]byte {
	return pool.Get().(*[]byte)
}

// Put returns b to the pool after resetting it to length zero.
func Put(b *[]byte) {
	*b = (*b)[:0]
	pool.Put(b)
}
