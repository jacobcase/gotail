//go:build !gotail_fsnotify

package watch

// NewFsnotify returns [ErrUnsupported] on builds without the gotail_fsnotify
// build tag. Enable with: go build -tags gotail_fsnotify
func NewFsnotify(c Config) (Watcher, error) {
	return nil, ErrUnsupported
}
