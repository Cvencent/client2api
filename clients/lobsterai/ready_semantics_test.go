package lobsterai

import (
	"testing"
	"time"
)

// #247：Ready 必须是「至少一个账号当前可选」——全部账号被操作员停用的平台
// 不得报 ready=true（与自己的「0/0 accounts ready」detail 同屏矛盾，平台页
// KPI 虚高）。
func TestAllDisabledAccountsDoNotReadReady(t *testing.T) {
	// 独立 pool：不依赖整个 Client 的构造（MaxInFlight 默认 0 = 不设上限，
	// selectable 只看 Enabled/token/冷却）。
	p := newPool()
	now := time.Now()
	// 两个账号，全部 Enabled=false（#244 封号案形状）。
	p.upsert(accountRecord{ID: "a1", AccessToken: "tok", Enabled: false})
	p.upsert(accountRecord{ID: "a2", AccessToken: "tok", Enabled: false})
	if p.hasSelectable(now, 2) {
		t.Fatal("all-disabled pool reports selectable: Ready would be true (issue #247)")
	}
	// 启用一个账号后 Ready 判据翻转。
	p.upsert(accountRecord{ID: "a1", AccessToken: "tok", Enabled: true})
	if !p.hasSelectable(now, 2) {
		t.Fatal("an enabled account with a token is not selectable: Ready would stay false")
	}
}
