package codearts

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// 钉住账号会话工作集感知（issue #189）。
//
// 01041 是厂商按账号计的「活跃会话工作集」上限（3 个），与 in_flight 无关：
// 响应结束后会话仍在厂商侧保温，粘性只保护回头会话——新对话线程仍会被 LRU
// 压给一个已持有 3 个保温会话的账号，白打一次 400 才发现满了。

func TestWorkingSetCountsDistinctSessionsOnly(t *testing.T) {
	ws := newSessionWorkingSet()
	now := time.Now()
	ws.recordSession("a1", "s1", now)
	ws.recordSession("a1", "s2", now)
	ws.recordSession("a1", "s3", now)
	if ws.distinct("a1", now) != 3 {
		t.Fatalf("distinct = %d, want 3", ws.distinct("a1", now))
	}
	// 粘性续轮不算新会话：同一 session id 重记只刷新时间戳。
	ws.recordSession("a1", "s1", now.Add(time.Minute))
	if ws.distinct("a1", now.Add(time.Minute)) != 3 {
		t.Fatalf("a continuation grew the set: distinct = %d, want 3（反向钉子：续轮不得计数，issue #189）", ws.distinct("a1", now.Add(time.Minute)))
	}
	if ws.wouldExceed("a1", "s1", now.Add(time.Minute)) {
		t.Error("continuation of a held session reported wouldExceed：粘性续轮必须豁免（issue #189）")
	}
	if !ws.wouldExceed("a1", "s4", now.Add(time.Minute)) {
		t.Error("a brand-new session on a full set reported room: 第 4 个会话必须被视作超限（issue #189）")
	}
}

func TestWorkingSetDecayAndFullPin(t *testing.T) {
	ws := newSessionWorkingSet()
	now := time.Now()
	for i := 0; i < 3; i++ {
		ws.recordSession("a1", string(rune('a'+i)), now)
	}
	if !ws.isFull("a1", now) {
		t.Fatal("three counted sessions did not read as full")
	}
	// 衰减窗过后自动腾出位置。
	later := now.Add(defaultSessionDecay + time.Minute)
	if ws.isFull("a1", later) {
		t.Fatal("the set stayed full past the decay window")
	}
	// 01041 真发生：置满优先于本地计数，按时钟衰减。
	ws2 := newSessionWorkingSet()
	pin := time.Now()
	ws2.pinFull("a2", pin)
	if !ws2.isFull("a2", pin.Add(time.Minute)) {
		t.Fatal("a pinned account did not read as full inside the decay clock")
	}
	if ws2.isFull("a2", pin.Add(defaultFullDecay+time.Minute)) {
		t.Fatal("the pin outlived its decay clock")
	}
	// 反向钉子：pin 是「厂商说了」，不凭空猜——普通 queue 文案不算。
	if isSessionCapRefusal("peak usage, try again after 10s") || isSessionCapRefusal("rate limit exceeded") {
		t.Error("普通 queue/限流文案被当成了会话上限拒绝：pin 只许 01041 触发（issue #189）")
	}
	if !isSessionCapRefusal(`{"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)"}`) {
		t.Error("01041 拒绝没有被识别：置满永远不会触发")
	}
}

func TestPickAccountDemotesAFullWorkingSet(t *testing.T) {
	c := affinityTestClient(t)
	now := time.Now()
	// a1 满工作集；a2 空。新会话必须先拿 a2。
	c.workingSet.recordSession("a1", "s-other-1", now)
	c.workingSet.recordSession("a1", "s-other-2", now)
	c.workingSet.recordSession("a1", "s-other-3", now)

	req := &core.ChatRequest{ConversationID: "conv-new"}
	e := c.pickAccount(req, nil, false)
	if e == nil || e.id() != "a2" {
		t.Fatalf("pickAccount = %v, want a2（满工作集账号必须降级，issue #189）", e)
	}

	// 全部账号都满：降级不得变成排除——普通 pick 兜底返回可用账号。
	c.workingSet.recordSession("a2", "s-other-1", now)
	c.workingSet.recordSession("a2", "s-other-2", now)
	c.workingSet.recordSession("a2", "s-other-3", now)
	e = c.pickAccount(req, nil, false)
	if e == nil {
		t.Fatal("pickAccount = nil when every account is full：降级不许变成排除（issue #189）")
	}
}

// 续轮不受工作集影响：会话已在集合里，wouldExceed=false，即账号被 pin 满
// 也不会把回头会话挡掉（厂商对已有会话的续轮不计数新增）。
func TestStickyContinuationBypassesTheFilter(t *testing.T) {
	c := affinityTestClient(t)
	now := time.Now()
	c.workingSet.recordSession("a1", "sess-1", now)
	// a1 持有 3 个会话：sess-1（本对话）+ 两个别人的。
	c.workingSet.recordSession("a1", "other-1", now)
	c.workingSet.recordSession("a1", "other-2", now)
	// 厂商 01041 判满。粘性解析把 conv-1 钉在 a1 上。
	c.workingSet.pinFull("a1", now)
	c.affinity.Bind("conv-1", "a1")
	// conversationKey 与 sessionIDFor 同源（ConversationID），所以绑定键即
	// conv id；会话集里已有 sess-1，续轮是 continued，不是新会话。
	req := &core.ChatRequest{ConversationID: "sess-1"}
	c.affinity.Bind("sess-1", "a1")
	e := c.pickAccount(req, nil, false)
	if e == nil || e.id() != "a1" {
		t.Fatalf("sticky continuation = %v, want a1：粘性续轮不得被工作集挡住（issue #189 反向钉子）", e)
	}
}
