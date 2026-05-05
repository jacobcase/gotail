// Package watch provides L1 primitives for tailing a file across rotation.
//
// Most callers want the higher-level [github.com/jacobcase/gotail/v2/tail]
// package, which adds checkpoint persistence, multi-file source enumeration,
// and lifecycle hooks on top of these primitives. Use watch directly only
// when you need a single-file watcher without those facilities.
//
// # Vocabulary
//
//   - [Position]: a pure value describing a coordinate in a file series —
//     which file (by path + inode) and how many bytes into it. No I/O.
//   - [Event]: a state transition emitted by a [Watcher]. Events describe
//     what happened; they do not carry byte data.
//   - [Watcher]: drives the state machine — detects new data, rotation, and
//     truncation via polling or fs notifications.
//   - [LineReader]: frames newline-delimited lines on top of a Watcher. It
//     opens its own file descriptor and owns its read buffer.
//
// # Backends
//
// [New] picks the best backend for the platform and build:
//
//   - [NewFsnotify] — inotify (Linux) / kqueue (macOS, FreeBSD, NetBSD,
//     OpenBSD). Sub-millisecond latency. Compiled in by default; opt out
//     with the `gotail_nofsnotify` build tag.
//   - [NewPolling] — interval-based stat polling. Always available. Used
//     as a fallback on Windows and on builds where fsnotify is excluded.
//
// # Buffer ownership
//
// The []byte slice returned by [LineReader.Next] is valid only until the next
// call to Next or Close. Callers that need to retain a line must copy it.
// This matches [bufio.Scanner.Bytes] semantics.
//
// # Concurrency
//
// A [Watcher] and the [LineReader] wrapped around it are not safe for
// concurrent use, including Close. Cancel the ctx passed to Next/Wait to
// unblock a pending call from another goroutine, then Close from the same
// goroutine that called Next.
//
// # Logging
//
// Every log line uses [log/slog] with consistent attribute keys:
// "path", "inode", "offset", "err".
//
// # Usage
//
//	w, err := watch.New(watch.Config{
//	    Path:     "/var/log/app.log",
//	    Interval: time.Second,
//	    Whence:   io.SeekEnd, // tail only new content
//	})
//	if err != nil { return err }
//	lr := watch.NewLineReader(w, watch.LineOptions{})
//	defer lr.Close()
//
//	for {
//	    line, _, err := lr.Next(ctx)
//	    if err != nil { return err }
//	    fmt.Printf("%s\n", line)
//	}
package watch
