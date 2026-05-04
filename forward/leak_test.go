package forward_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after the suite to catch goroutine leaks. The
// forward package owns the feeder goroutine wired to RecordSource and is
// the most likely site for a regression that resurrects the leak fixed
// in Issue #8 (CODE_REVIEW.md).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
