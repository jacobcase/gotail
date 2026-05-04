package tail

import "errors"

var (
	// ErrSourceExhausted is returned by Next when StopAtEOF is set and the
	// active file has been fully consumed.
	ErrSourceExhausted = errors.New("tail: source exhausted (StopAtEOF)")

	// ErrCheckpointMissing is returned by New when the loaded checkpoint
	// references a file no longer present and OnMissingCheckpoint is Fail.
	ErrCheckpointMissing = errors.New("tail: checkpointed file no longer present")

	// ErrLockHeld is returned by NewFileCursor when the sibling lock file is
	// already held by another process. Requires Phase 3 WithFlock option.
	ErrLockHeld = errors.New("tail: cursor lock held by another process")
)
