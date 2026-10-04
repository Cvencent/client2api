package panel

import (
	"strings"
	"testing"
)

// The recent-call table must distinguish a failed candidate attempt from a
// final request failure; otherwise the platform alert and the usage page still
// look inconsistent.
func TestRecentCallsLabelCandidateFailures(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{"x.attempt", "候选失败"} {
		if !strings.Contains(src, want) {
			t.Errorf("recent-call renderer does not mention %q", want)
		}
	}
}
