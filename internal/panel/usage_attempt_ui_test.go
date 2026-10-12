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

// Operator-facing failure rows must say why they failed.  The gateway stores
// that in failure_reason; the table must render the same key and keep the
// header width aligned with the row template.
func TestRecentCallsShowFailureReason(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, "失败原因") {
		t.Error("最近调用的表头没有「失败原因」列")
	}
	body := poolStatsFuncBody(t, src, "renderRecentRows")
	if !strings.Contains(body, "x.failure_reason") {
		t.Error("renderRecentRows 没有读取 x.failure_reason：失败原因列永远是空的")
	}
	if h, r := recentHeaderCells(t, src), recentRowCells(t, src); h != r {
		t.Errorf("最近调用表头 %d 列、行模板 %d 格：新增失败原因列后整行会错位", h, r)
	}
}
