//go:build !unix && !windows

package tail_test

import "testing"

func testFlockConflict(t *testing.T)        { t.Skip("flock not supported on this platform") }
func testFlockReleasedOnClose(t *testing.T) { t.Skip("flock not supported on this platform") }
func testFlockPIDInFile(t *testing.T)       { t.Skip("flock not supported on this platform") }
func testFlockCrossProcess(t *testing.T)    { t.Skip("flock not supported on this platform") }
