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
