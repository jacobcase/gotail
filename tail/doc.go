// Package tail provides durable, line-oriented file tailing with persistent
// checkpoints, structured logging, and rotation support.
//
// # Vocabulary
//
//   - [Position] — an alias for [watch.Position]: a triple of {file path,
//     inode, byte offset} that uniquely identifies a point in a file series.
//   - [Checkpoint] — a persisted record: a [Position] plus optional opaque
//     user metadata (JSON). What the storage port reads and writes.
//   - [Cursor] — the storage port: an interface for loading and saving
//     [Checkpoint] values. [NewFileCursor] provides an atomic, fsync-safe
//     implementation; [NewMemoryCursor] is an in-memory stub for tests.
//
// # Modes
//
// Live tail (default): the [Tailer] follows the file indefinitely, surviving
// rotation and truncation. Call Close to stop.
//
// Backfill (StopAtEOF: true): the [Tailer] drains the file to end-of-file,
// closes the [Done] channel, and subsequent [Tailer.Next] calls return
// [ErrSourceExhausted].
//
// # Cursor durability
//
// [FileCursor] writes checkpoints atomically: data is written to a ".tmp" file,
// fsynced, then renamed over the final path. An optional directory fsync
// (default on) makes the rename itself durable against power loss.
//
// # On-disk format
//
// Checkpoints are JSON. The schema is a wire-format commitment — files
// written by one version of gotail must remain readable by future versions.
// Notable details:
//
//   - The 64-bit fields of [Position] (Inode, Offset) are encoded as quoted
//     strings (`json:"...,string"`). Many JSON consumers parse numbers as
//     IEEE-754 doubles, which silently lose precision past 2^53. Quoting
//     them preserves full int64 fidelity.
//   - [Checkpoint.Version] is written as 1 today and validated on Load;
//     bumping it requires migration support in future versions.
//   - [Checkpoint.Meta] is opaque [json.RawMessage] passed through verbatim.
//     Schema discipline for Meta is the caller's responsibility.
//
// # Usage
//
//	cur, _ := tail.NewFileCursor("/var/run/myapp.cursor")
//	t, _ := tail.New(tail.Options{
//	    Source: tail.SingleFile("/var/log/app.log"),
//	    Cursor: cur,
//	    Interval: time.Second,
//	})
//	defer t.Close()
//
//	for rec, err := range t.Records(ctx) {
//	    if err != nil { break }
//	    process(rec.Line)
//	    t.Commit(ctx, rec.Pos)
//	}
package tail
