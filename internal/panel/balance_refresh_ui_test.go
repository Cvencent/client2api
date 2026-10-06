package panel

import (
	"strings"
	"testing"
)

// These checks keep the account-pool controls honest: a refresh must display
// the server's progress or throttle, and a batch check-in must show which
// account is currently running instead of looking like a dead button.
func TestBalanceRefreshUIShowsServerProgress(t *testing.T) {
	src := string(indexHTML)
	apply := poolStatsFuncBody(t, src, "applyBalances")
	if !strings.Contains(apply, "data.refresh") {
		t.Error("applyBalances 没有保存服务端返回的刷新进度")
	}
	poll := poolStatsFuncBody(t, src, "pollBalanceCache")
	if !strings.Contains(poll, "st && st.running") {
		t.Error("pollBalanceCache 没有在服务端仍显示 running 时继续回读")
	}
	bulk := poolStatsFuncBody(t, src, "bulk")
	if !strings.Contains(bulk, "loadBalances(CUR, true)") {
		t.Error("刷新余额按钮不再触发服务端刷新")
	}
	if !strings.Contains(bulk, "r.data.refresh") && !strings.Contains(bulk, "loadBalances(CUR, true)") {
		t.Error("刷新余额按钮没有读取服务端的状态说明")
	}
}

func TestCheckinUIShowsPerAccountProgress(t *testing.T) {
	body := poolStatsFuncBody(t, string(indexHTML), "checkinClient")
	for _, want := range []string{
		`$("#btnCheckinAll")`,
		`done + "/" + list.length`,
		"已签到",
		"失败",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("checkinClient 缺少批量签到进度标记 %q", want)
		}
	}
}
