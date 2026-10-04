package gateway

import (
	"os"
	"testing"
)

// TestMain silences the per-request console rows for the whole test binary.
// They go to os.Stdout by default, where a failing assertion would be buried
// under rows written by every other test in the package.  The tests that do
// assert on the rows turn them back on through captureChatRows.
func TestMain(m *testing.M) {
	chatLogEnabled = false
	os.Exit(m.Run())
}
