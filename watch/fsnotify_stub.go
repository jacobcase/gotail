//go:build gotail_nofsnotify || !(linux || darwin || freebsd || netbsd || openbsd)

package watch

// NewFsnotify returns [ErrUnsupported]. This stub is selected when either:
//   - the gotail_nofsnotify build tag is set (explicit opt-out), or
//   - the platform has no fsnotify backend in this module (e.g. Windows).
//
// Callers should use [New], which falls back to polling when fsnotify is
// unavailable.
func NewFsnotify(c Config) (Watcher, error) {
	return nil, ErrUnsupported
}
