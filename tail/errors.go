package tail

import (
	"errors"

	"github.com/jacobcase/gotail/v2/watch"
)

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

	// ErrInodeMismatch is returned when a checkpoint's inode no longer
	// matches the file at its path. Reachable when WithFailOnInodeMismatch
	// is set or when an Options.OnInodeMismatch hook has otherwise been
	// wired to fail the resume. Aliased from [watch.ErrInodeMismatch] so
	// callers can check with errors.Is at the L2 surface.
	ErrInodeMismatch = watch.ErrInodeMismatch
)
