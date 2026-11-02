package openaicompat

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// officeace 的 invalid 量产（issue #104 的 2026-11-02 重开）。
//
// 现场：officeace invalid 9 / ready 3，其中至少 4 个在被误标时**是有余额的**
// （45 ~ 3.3k 积分）。它们的 401 是 IAM 竞态形状，而当时的否决只认「最近一次读数
// 恰好为 0」，于是这些行落进 kindAuth → 写 invalid → 自锁（选择器跳过、
// 两个 sweep 也跳过，能清标记的 200 永远打不出去）。
//
// 操作员的方向：**读数有值即豁免 invalid**（0 走 quota、>0 也走 quota 短冷却），
// 因为这家厂商的 IAM 401 从来不是模型 key 死亡（key 是长期 App Key / App Secret）。
//
// 判别信号只能是「这个账号有没有过读数」，不能是拒绝文本：这家厂商对**耗尽的**
// 订阅回的就是同一句 `AppKey or AppSecret is invalid` + APIG.1009（premium 测试里
// 的 officeAceDrainedSubscriptionJSON 就是这个形状），文本无法区分两者。

// TestOfficeAceVetoCoversAnyCreditPosition 钉住重开后的语义：只要有过读数（0 /
// 正数 / 不限量），auth 形状的拒绝都走 quota；唯独「从未读过」保持死 key 路径。
func TestOfficeAceVetoCoversAnyCreditPosition(t *testing.T) {
	o := &officeAceSource{source: &source{now: time.Now, cooling: map[string]time.Time{}}}
	now := time.Now()

	if o.officeAceRefusalIsCredits("never-read") {
		t.Fatal("从未读过余额的账号被豁免了：这条路径必须留作死 key（账号异常 + 重新导入）")
	}

	for _, pos := range []int64{0, 1, 45, 3300, -1} {
		o.noteOfficeAcePoints("a", pos, now)
		if !o.officeAceRefusalIsCredits("a") {
			t.Errorf("读数 %d 的账号没有被豁免：它的 401 会写成 invalid 并自锁（issue #104）", pos)
		}
	}
}

// TestOfficeAceVetoReadsTheRefusalText 钉住「文本不是判别信号」这件事本身：这家
// 厂商对耗尽的订阅回的就是那句 AppKey / AppSecret 文本，所以任何按消息内容把
// 它判回死 key 的规则都会重新冻结 #57 的耗尽路径。
func TestOfficeAceVetoReadsTheRefusalText(t *testing.T) {
	o := &officeAceSource{source: &source{now: time.Now, cooling: map[string]time.Time{}}}
	o.noteOfficeAcePoints("drained", 0, time.Now())
	// 这正是 fixture 里代表「耗尽」的那一句（与 premium 测试共用的形状）。
	if !o.officeAceRefusalIsCredits("drained") {
		t.Fatal("耗尽的账号被按拒绝文本判回了死 key：租借包烧穿会被标成「账号异常」并自锁（issue #57/#104）")
	}
}

// TestOfficeAceVetoKeepsFundedRowsOutOfInvalid 是端到端形态：有余额的账号走到
// chat 401 之后，行必须留在可用/冷却路径上，而不是 invalid。
func TestOfficeAceVetoKeepsFundedRowsOutOfInvalid(t *testing.T) {
	var chatCalls atomic.Int32
	// 订阅里有一笔余额（5300 额度、用了 0），chat 仍然回 401 —— 操作员量到的
	// 「正积分账号 IAM 竞态」形状。
	funded := `{
  "subscribe_status": "SUBSCRIBED",
  "skus": [{
    "sku_code": "officeace.personal.package.trial",
    "subscribe_status": "SUBSCRIBED",
    "quotas": [
      {"sku_attr_code": "officeace_points", "sku_value": "5300", "current_value": "0",
       "effective_time": "2026-10-16T09:21:49.739Z", "expired_time": "2099-01-01T00:00:00.000Z"}
    ]
  }],
  "bonus_skus": []
}`
	srv := officeAceChatServer(t, funded, &chatCalls)
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddChatAccount(t, o, srv)
	if _, err := o.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("balance: %v", err)
	}
	if err := officeAceChatOnce(t, o.source); err == nil {
		t.Fatal("chat against a refused key unexpectedly succeeded")
	}
	records, err := o.source.Accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	for _, a := range records {
		if a.ID != id {
			continue
		}
		if a.State == "invalid" {
			t.Fatalf("有余额的账号被 401 标成了 invalid：自锁后两个 sweep 与选择器都跳过它，能清标记的 200 永远打不出去（issue #104）")
		}
	}
}
