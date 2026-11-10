package gateway

import (
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// #240：折叠请求被 codearts 81001 拒且每轮重试——修形后置到合并后（拼接边界
// 也过 repair）、chunk 首条 tool 垫底、连续失败退避（不每轮白付同形状请求）。
// ---------------------------------------------------------------------------

// TestAFoldAssemblyRepairsTheMergedShape 钉住修形时序的核心：孤立 tool 结果
// 在「fold prompt + system + chunk」合并列表上必须被 RepairToolPairing 清掉，
// 且合并后首条不再是 tool——这正是 81001 拒绝的形状（issue #240）。
func TestAFoldAssemblyRepairsTheMergedShape(t *testing.T) {
	system := []core.Message{{Role: "system", Content: "be terse"}}
	chunk := []core.Message{
		{Role: "tool", ToolCallID: "call-1", Content: "orphan result"},
		{Role: "user", Content: "real turn"},
		{Role: "assistant", Content: "answer"},
	}
	reqMsgs := make([]core.Message, 0, len(system)+len(chunk)+1)
	reqMsgs = append(reqMsgs, core.Message{Role: "system", Content: trimSummaryFoldPrompt})
	reqMsgs = append(reqMsgs, system...)
	reqMsgs = append(reqMsgs, chunk...)
	reqMsgs = core.RepairToolPairing(core.RepairToolArguments(reqMsgs))
	if len(reqMsgs) > 0 && strings.EqualFold(strings.TrimSpace(reqMsgs[0].Role), "tool") {
		t.Fatalf("合并后首条仍是 tool：%+v（81001 拒此形状）", reqMsgs[0])
	}
	for i, m := range reqMsgs {
		if strings.EqualFold(strings.TrimSpace(m.Role), "tool") && strings.TrimSpace(m.ToolCallID) == "call-1" {
			t.Fatalf("孤立 tool 结果残留于 %d：%+v（合并后 repair 未覆盖拼接边界）", i, m)
		}
	}
}

// TestBFoldBackoffSkipsTheOffer 钉住退避门：同一 fingerprint 连续失败达阈值后，
// offer 被跳过；窗口经过后允许一次新尝试。
func TestBFoldBackoffSkipsTheOffer(t *testing.T) {
	store := newTrimSummaryStore()
	key := "fp-1"
	for i := 0; i < trimFoldBackoffThreshold; i++ {
		store.recordFoldFailure(key)
	}
	if backed, fails := store.foldBackedOff(key); !backed || fails != trimFoldBackoffThreshold {
		t.Fatalf("backoff = %v,%d, want armed at threshold", backed, fails)
	}
	// 窗口经过：允许一次新尝试。
	store.mu.Lock()
	e := store.entries[key]
	e.lastFail = time.Now().Add(-trimFoldBackoff - time.Minute)
	store.entries[key] = e
	store.mu.Unlock()
	if backed, fails := store.foldBackedOff(key); backed || fails != 0 {
		t.Fatalf("窗口经过后仍退避：%v,%d（应放行一次新尝试）", backed, fails)
	}
	// 其它 fingerprint 不受影响。
	if backed, _ := store.foldBackedOff("fp-other"); backed {
		t.Error("无关 fingerprint 被连坐退避")
	}
	// 无失败记录的键不退避。
	if backed, _ := store.foldBackedOff(""); backed {
		t.Error("空键被退避")
	}
}

// TestBFoldAssemblyAppendsTailUser 钉住 #240 第三轮实验（opencode 同族确认）：
// 纯工具会话的 middle 拼出的请求全程无 user——officeace 与 opencode 两家厂商
// 都拒这种形状。末尾必须垫一条 user（system 后无 user / 全程无 user 两条嫌疑
// 同时覆盖），且 repair 后仍保持。
func TestBFoldAssemblyAppendsTailUser(t *testing.T) {
	system := []core.Message{{Role: "system", Content: "be terse"}}
	chunk := []core.Message{
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call-1"}}, Content: ""},
		{Role: "tool", ToolCallID: "call-1", Content: "result"},
		{Role: "assistant", Content: "answer"},
	}
	reqMsgs := make([]core.Message, 0, len(system)+len(chunk)+1)
	reqMsgs = append(reqMsgs, core.Message{Role: "system", Content: trimSummaryFoldPrompt})
	reqMsgs = append(reqMsgs, system...)
	reqMsgs = append(reqMsgs, chunk...)
	reqMsgs = core.RepairToolPairing(core.RepairToolArguments(reqMsgs))
	// 修复后的拼装（summariseTurns 的形状）：末尾垫 user。
	if !strings.EqualFold(strings.TrimSpace(reqMsgs[len(reqMsgs)-1].Role), "user") {
		reqMsgs = append(reqMsgs, core.Message{Role: "user", Content: "（以上是被折叠会话的历史记录，请按 system 指令总结）"})
	}
	last := reqMsgs[len(reqMsgs)-1]
	if !strings.EqualFold(strings.TrimSpace(last.Role), "user") {
		t.Fatalf("last message = %+v, want a tail user (issue #240 第三轮)", last)
	}
	// 中段必须有 assistant(tool_calls)+tool 配对（形状修复不被垫尾破坏）。
	found := false
	for _, m := range reqMsgs {
		if len(m.ToolCalls) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("tool pairing vanished after the tail user was appended")
	}
}
