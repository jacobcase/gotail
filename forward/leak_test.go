package forward_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after the suite to catch goroutine leaks. After
// the H1 refactor, Forwarder.Run owns no goroutines of its own — but
// downstream code (Tailer, Watcher) does, and this guards against
// regressions there leaking through forward integration tests.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
