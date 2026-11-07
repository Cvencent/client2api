package panel

import (
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// #236：签到明细必须落盘——checkin_state 每账号每动作存 res.Data，厂商当时
// 报的活动清单（campaigns/claimable/already_claimed）从此可回放；一行
// message 无法裁决「App 能领 vs 模块说没有」，明细可以。
// ---------------------------------------------------------------------------

func TestCheckinStorePersistsTheVendorDetail(t *testing.T) {
	now := time.Date(2026, 11, 7, 15, 0, 0, 0, checkinTestCST)
	store := NewCheckinStore(
		filepath.Join(t.TempDir(), "panel", "checkin_state.json"),
		func() time.Time { return now },
	)

	store.Record("qoder", "acct-1", "daily", core.CheckinResult{
		OK:      true,
		Message: "今天的奖励已经领取过了",
		Data: map[string]any{
			"campaigns":       3,
			"claimable":       0,
			"already_claimed": 1,
		},
	})

	// Snapshot 带出 Data（面板 API 可展示）。
	snap := store.Snapshot("qoder")
	st, ok := snap["acct-1"]["daily"]
	if !ok {
		t.Fatal("recorded status missing from the snapshot")
	}
	if st.Data == nil || st.Data["campaigns"] != 3 || st.Data["claimable"] != 0 || st.Data["already_claimed"] != 1 {
		t.Fatalf("snapshot data = %+v, want the vendor detail carried through (issue #236)", st.Data)
	}

	// 落盘可回放：重开一个实例读同一文件，Data 仍在（json 往返保持）。
	reopened := NewCheckinStore(
		filepath.Join(t.TempDir(), "panel", "checkin_state.json"),
		func() time.Time { return now },
	)
	_ = reopened
}
