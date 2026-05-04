// Package watch provides L1 primitives for tailing a file across rotation.
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
// # Buffer ownership
//
// The []byte slice returned by [LineReader.Next] is valid only until the next
// call to Next or Close. Callers that need to retain a line must copy it.
// This matches [bufio.Scanner.Bytes] semantics.
//
// # Logging
//
// Every log line uses [log/slog] with consistent attribute keys:
// "path", "inode", "offset", "err".
package watch
