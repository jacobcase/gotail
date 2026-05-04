package tail_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after the suite. The tail package owns the
// LineReader and Tailer lifecycles; this catches regressions where a
// blocked Next or rotation handler outlives Close.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
