package scheduler

import (
	"strings"
	"testing"

	"client2api/internal/core"
)

// #236：refused/skipped 日志行必须带厂商明细计数——「campaigns=3
// claimable=0 already_claimed=1」直接裁决 App 能领 vs 模块说没有。
func TestCheckinDetailRendersTheVendorNumbers(t *testing.T) {
	res := core.CheckinResult{Data: map[string]any{
		"campaigns":       3,
		"claimable":       0,
		"already_claimed": 1,
	}}
	got := checkinDetail(res)
	for _, want := range []string{"already_claimed=1", "campaigns=3", "claimable=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail 缺 %q：%s", want, got)
		}
	}
	if !strings.HasPrefix(got, " [") {
		t.Errorf("detail 形状不对：%s", got)
	}
	if checkinDetail(core.CheckinResult{}) != "" {
		t.Error("无 Data 时打了空明细：轮询常态必须安静")
	}
}
